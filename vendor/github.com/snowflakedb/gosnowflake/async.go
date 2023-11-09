// Copyright (c) 2021-2022 Snowflake Computing Inc. All rights reserved.

package gosnowflake

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

func (sr *snowflakeRestful) processAsync(
	ctx context.Context,
	respd *execResponse,
	headers map[string]string,
	timeout time.Duration,
	cfg *Config) (*execResponse, error) {
	// placeholder object to return to user while retrieving results
	rows := new(snowflakeRows)
	res := new(snowflakeResult)
	switch resType := getResultType(ctx); resType {
	case execResultType:
		res.queryID = respd.Data.QueryID
		res.status = QueryStatusInProgress
		res.errChannel = make(chan error)
		respd.Data.AsyncResult = res
	case queryResultType:
		rows.queryID = respd.Data.QueryID
		rows.status = QueryStatusInProgress
		rows.errChannel = make(chan error)
		respd.Data.AsyncRows = rows
	default:
		return respd, nil
	}

	// spawn goroutine to retrieve asynchronous results
	go sr.getAsync(ctx, headers, sr.getFullURL(respd.Data.GetResultURL, nil), timeout, res, rows, cfg)
	return respd, nil
}

func (sr *snowflakeRestful) getAsync(
	ctx context.Context,
	headers map[string]string,
	URL *url.URL,
	timeout time.Duration,
	res *snowflakeResult,
	rows *snowflakeRows,
	cfg *Config) error {
	resType := getResultType(ctx)
	var errChannel chan error
	sfError := &SnowflakeError{
		Number: ErrAsync,
	}
	if resType == execResultType {
		errChannel = res.errChannel
		sfError.QueryID = res.queryID
	} else {
		errChannel = rows.errChannel
		sfError.QueryID = rows.queryID
	}
	defer close(errChannel)
	token, _, _ := sr.TokenAccessor.GetTokens()
	headers[headerAuthorizationKey] = fmt.Sprintf(headerSnowflakeToken, token)

	var err error
	var respd execResponse
	retry := 0
	retryPattern := []int32{1, 1, 2, 3, 4, 8, 10}

	for {
		resp, err := sr.FuncGet(ctx, sr, URL, headers, timeout)
		if err != nil {
			logger.WithContext(ctx).Errorf("failed to get response. err: %v", err)
			sfError.Message = err.Error()
			errChannel <- sfError
			return err
		}
		defer resp.Body.Close()

		respd = execResponse{} // reset the response
		err = json.NewDecoder(resp.Body).Decode(&respd)
		if err != nil {
			logger.WithContext(ctx).Errorf("failed to decode JSON. err: %v", err)
			sfError.Message = err.Error()
			errChannel <- sfError
			return err
		}
		if respd.Code != queryInProgressAsyncCode {
			// If the query takes longer than 45 seconds to complete the results are not returned.
			// If the query is still in progress after 45 seconds, retry the request to the /results endpoint.
			// For all other scenarios continue processing results response
			break
		} else {
			// Sleep before retrying get result request. Exponential backoff up to 5 seconds.
			// Once 5 second backoff is reached it will keep retrying with this sleeptime.
			sleepTime := time.Millisecond * time.Duration(500*retryPattern[retry])
			logger.WithContext(ctx).Infof("Query execution still in progress. Sleep for %v ms", sleepTime)
			time.Sleep(sleepTime)
		}
		if retry < len(retryPattern)-1 {
			retry++
		}

	}

	sc := &snowflakeConn{rest: sr, cfg: cfg, queryContextCache: (&queryContextCache{}).init(), currentTimeProvider: defaultTimeProvider}
	if respd.Success {
		if resType == execResultType {
			res.insertID = -1
			if isDml(respd.Data.StatementTypeID) {
				res.affectedRows, err = updateRows(respd.Data)
				if err != nil {
					return err
				}
			} else if isMultiStmt(&respd.Data) {
				r, err := sc.handleMultiExec(ctx, respd.Data)
				if err != nil {
					res.errChannel <- err
					return err
				}
				res.affectedRows, err = r.RowsAffected()
				if err != nil {
					res.errChannel <- err
					return err
				}
			}
			res.queryID = respd.Data.QueryID
			res.errChannel <- nil // mark exec status complete
		} else {
			rows.sc = sc
			rows.queryID = respd.Data.QueryID
			if isMultiStmt(&respd.Data) {
				if err = sc.handleMultiQuery(ctx, respd.Data, rows); err != nil {
					rows.errChannel <- err
					return err
				}
			} else {
				rows.addDownloader(populateChunkDownloader(ctx, sc, respd.Data))
			}
			rows.ChunkDownloader.start()
			rows.errChannel <- nil // mark query status complete
		}
	} else {
		var code int
		if respd.Code != "" {
			code, err = strconv.Atoi(respd.Code)
			if err != nil {
				code = -1
			}
		} else {
			code = -1
		}
		errChannel <- &SnowflakeError{
			Number:   code,
			SQLState: respd.Data.SQLState,
			Message:  respd.Message,
			QueryID:  respd.Data.QueryID,
		}
	}
	return nil
}
