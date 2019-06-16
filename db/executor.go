// Copyright © 2016 Abcum Ltd
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package db

import (
	"time"

	"context"

	"runtime/debug"

	"github.com/abcum/surreal/kvs"
	"github.com/abcum/surreal/log"
	"github.com/abcum/surreal/mem"
	"github.com/abcum/surreal/sql"
)

type executor struct {
	id    string
	ns    string
	db    string
	dbo   *mem.Cache
	time  int64
	lock  *mutex
	opts  *options
	send  chan *Response
	cache *cache
}

func newExecutor(id, ns, db string) (e *executor) {

	e = executorPool.Get().(*executor)

	e.id = id
	e.ns = ns
	e.db = db

	e.dbo = mem.New()

	e.opts = newOptions()

	e.send = make(chan *Response)

	e.cache = new(cache)

	return

}

func (e *executor) execute(ctx context.Context, ast *sql.Query) {

	// Ensure that the executor is added back into
	// the executor pool when the executor has
	// finished processing the request.

	defer executorPool.Put(e)

	// Ensure that the query responses channel is
	// closed when the full query has been processed
	// and dealt with.

	defer close(e.send)

	// If we are making use of a global transaction
	// which is not committed at the end of the
	// query set, then cancel the transaction.

	defer func() {
		if e.dbo.TX != nil {
			e.dbo.Cancel()
			clear(e.id)
		}
	}()

	// If we have panicked during query execution
	// then ensure that we recover from the error
	// and print the error to the log.

	defer func() {
		if err := recover(); err != nil {
			log.WithPrefix(logKeyDB).WithFields(map[string]interface{}{
				logKeyId: e.id, logKeyStack: string(debug.Stack()),
			}).Errorln(err)
		}
	}()

	// Loop over the defined query statements and
	// process them, while listening for the quit
	// channel to see if the client has gone away.

	for _, stm := range ast.Statements {
		select {
		case <-ctx.Done():
			return
		default:
			e.conduct(ctx, stm)
		}
	}

}

func (e *executor) conduct(ctx context.Context, stm sql.Statement) {

	var err error
	var now time.Time
	var rsp *Response
	var buf []*Response
	var res []interface{}

	// When in debugging mode, log every sql
	// query, along with the query execution
	// speed, so we can analyse slow queries.

	log := log.WithPrefix(logKeySql).WithFields(map[string]interface{}{
		logKeyId:   e.id,
		logKeyKind: ctx.Value(ctxKeyKind),
		logKeyVars: ctx.Value(ctxKeyVars),
	})

	if len(e.ns) != 0 {
		log = log.WithField(logKeyNS, e.ns)
	}

	if len(e.db) != 0 {
		log = log.WithField(logKeyDB, e.db)
	}

	// If we are not inside a global transaction
	// then reset the error to nil so that the
	// next statement is not ignored.

	if e.dbo.TX == nil {
		err, now = nil, time.Now()
	}

	// Check to see if the current statement is
	// a TRANSACTION statement, and if it is
	// then deal with it and move on to the next.

	switch stm.(type) {
	case *sql.BeginStatement:
		e.lock = new(mutex)
		err = e.begin(ctx, true)
		return
	case *sql.CancelStatement:
		err, buf = e.cancel(buf, err, e.send)
		if err != nil {
			clear(e.id)
		} else {
			clear(e.id)
		}
		return
	case *sql.CommitStatement:
		err, buf = e.commit(buf, err, e.send)
		if err != nil {
			clear(e.id)
		} else {
			flush(e.id)
		}
		return
	}

	// If an error has occured and we are inside
	// a global transaction, then ignore all
	// subsequent statements in the transaction.

	if err == nil {
		res, err = e.operate(ctx, stm)
	} else {
		res, err = []interface{}{}, errQueryNotExecuted
	}

	rsp = &Response{
		Time:   time.Since(now).String(),
		Status: status(err),
		Detail: detail(err),
		Result: append([]interface{}{}, res...),
	}

	// Log the sql statement along with the
	// query duration time, and mark it as
	// an error if the query failed.

	switch err.(type) {
	default:
		log.WithFields(map[string]interface{}{
			logKeyTime: time.Since(now).String(),
		}).Debugln(stm)
	case error:
		log.WithFields(map[string]interface{}{
			logKeyTime:  time.Since(now).String(),
			logKeyError: detail(err),
		}).Errorln(stm)
	}

	// If we are not inside a global transaction
	// then we can output the statement response
	// immediately to the channel.

	if e.dbo.TX == nil {
		e.send <- rsp
	}

	// If we are inside a global transaction we
	// must buffer the responses for output at
	// the end of the transaction.

	if e.dbo.TX != nil {
		switch stm.(type) {
		case *sql.ReturnStatement:
			buf = groupd(buf, rsp)
		default:
			buf = append(buf, rsp)
		}
	}

}

func (e *executor) operate(ctx context.Context, stm sql.Statement) (res []interface{}, err error) {

	var loc bool
	var trw bool
	var canc context.CancelFunc

	// If we are not inside a global transaction
	// then grab a new transaction, ensuring that
	// it is closed at the end.

	if e.dbo.TX == nil {

		loc = true

		switch stm := stm.(type) {
		case sql.WriteableStatement:
			trw = stm.Writeable()
		default:
			trw = false
		}

		err = e.begin(ctx, trw)
		if err != nil {
			return
		}

		defer e.dbo.Cancel()

		// Let's create a new mutex for just this
		// local transaction, so we can track any
		// recursive queries and race errors.

		e.lock = new(mutex)

	}

	// Mark the beginning of this statement so we
	// can monitor the running time, and ensure
	// it runs no longer than specified.

	if stm, ok := stm.(sql.KillableStatement); ok {
		if stm.Duration() > 0 {
			ctx, canc = context.WithTimeout(ctx, stm.Duration())
			defer func() {
				if tim := ctx.Err(); err == nil && tim != nil {
					res, err = nil, &TimerError{timer: stm.Duration()}
				}
				canc()
			}()
		}
	}

	// Specify a new time for the current executor
	// iteration, so that all subqueries and async
	// events are saved with the same version time.

	e.time = time.Now().UnixNano()

	// Execute the defined statement, receiving the
	// result set, and any errors which occured
	// while processing the query.

	switch stm := stm.(type) {

	case *sql.OptStatement:
		res, err = e.executeOpt(ctx, stm)

	case *sql.UseStatement:
		res, err = e.executeUse(ctx, stm)

	case *sql.RunStatement:
		res, err = e.executeRun(ctx, stm)

	case *sql.InfoStatement:
		res, err = e.executeInfo(ctx, stm)

	case *sql.LetStatement:
		res, err = e.executeLet(ctx, stm)
	case *sql.ReturnStatement:
		res, err = e.executeReturn(ctx, stm)

	case *sql.LiveStatement:
		res, err = e.executeLive(ctx, stm)
	case *sql.KillStatement:
		res, err = e.executeKill(ctx, stm)

	case *sql.IfelseStatement:
		res, err = e.executeIfelse(ctx, stm)
	case *sql.SelectStatement:
		res, err = e.executeSelect(ctx, stm)
	case *sql.CreateStatement:
		res, err = e.executeCreate(ctx, stm)
	case *sql.UpdateStatement:
		res, err = e.executeUpdate(ctx, stm)
	case *sql.DeleteStatement:
		res, err = e.executeDelete(ctx, stm)
	case *sql.RelateStatement:
		res, err = e.executeRelate(ctx, stm)

	case *sql.InsertStatement:
		res, err = e.executeInsert(ctx, stm)
	case *sql.UpsertStatement:
		res, err = e.executeUpsert(ctx, stm)

	case *sql.DefineNamespaceStatement:
		res, err = e.executeDefineNamespace(ctx, stm)
	case *sql.RemoveNamespaceStatement:
		res, err = e.executeRemoveNamespace(ctx, stm)

	case *sql.DefineDatabaseStatement:
		res, err = e.executeDefineDatabase(ctx, stm)
	case *sql.RemoveDatabaseStatement:
		res, err = e.executeRemoveDatabase(ctx, stm)

	case *sql.DefineLoginStatement:
		res, err = e.executeDefineLogin(ctx, stm)
	case *sql.RemoveLoginStatement:
		res, err = e.executeRemoveLogin(ctx, stm)

	case *sql.DefineTokenStatement:
		res, err = e.executeDefineToken(ctx, stm)
	case *sql.RemoveTokenStatement:
		res, err = e.executeRemoveToken(ctx, stm)

	case *sql.DefineScopeStatement:
		res, err = e.executeDefineScope(ctx, stm)
	case *sql.RemoveScopeStatement:
		res, err = e.executeRemoveScope(ctx, stm)

	case *sql.DefineTableStatement:
		res, err = e.executeDefineTable(ctx, stm)
	case *sql.RemoveTableStatement:
		res, err = e.executeRemoveTable(ctx, stm)

	case *sql.DefineEventStatement:
		res, err = e.executeDefineEvent(ctx, stm)
	case *sql.RemoveEventStatement:
		res, err = e.executeRemoveEvent(ctx, stm)

	case *sql.DefineFieldStatement:
		res, err = e.executeDefineField(ctx, stm)
	case *sql.RemoveFieldStatement:
		res, err = e.executeRemoveField(ctx, stm)

	case *sql.DefineIndexStatement:
		res, err = e.executeDefineIndex(ctx, stm)
	case *sql.RemoveIndexStatement:
		res, err = e.executeRemoveIndex(ctx, stm)

	}

	// If the context is already closed or failed,
	// then ignore this result, clear all queued
	// changes, and reset the transaction.

	select {

	case <-ctx.Done():

		e.dbo.Cancel()
		e.dbo.Reset()
		clear(e.id)

	default:

		// If this is a local transaction for only the
		// current statement, then commit or cancel
		// depending on the result error.

		if loc && e.dbo.Closed() == false {

			// As this is a local transaction then
			// make sure we reset the transaction
			// context.

			defer e.dbo.Reset()

			// If there was an error with the query
			// then clear the queued changes and
			// return immediately.

			if err != nil {
				e.dbo.Cancel()
				clear(e.id)
				return
			}

			// Otherwise check if this is a read or
			// a write transaction, and attempt to
			// Cancel or Commit, returning any errors.

			if !trw {
				if err = e.dbo.Cancel(); err != nil {
					clear(e.id)
				} else {
					clear(e.id)
				}
			} else {
				if err = e.dbo.Commit(); err != nil {
					clear(e.id)
				} else {
					flush(e.id)
				}
			}

		}

	}

	return

}

func (e *executor) begin(ctx context.Context, rw bool) (err error) {
	if e.dbo.TX == nil {
		e.dbo = mem.New()
		e.dbo.TX, err = db.Begin(ctx, rw)
	}
	return
}

func (e *executor) cancel(buf []*Response, err error, chn chan<- *Response) (error, []*Response) {

	defer e.dbo.Reset()

	if e.dbo.TX == nil {
		return nil, buf
	}

	err = e.dbo.Cancel()

	for _, v := range buf {
		v.Status = "ERR"
		v.Result = []interface{}{}
		v.Detail = "Transaction cancelled"
		chn <- v
	}

	for i := len(buf) - 1; i >= 0; i-- {
		buf[len(buf)-1] = nil
		buf = buf[:len(buf)-1]
	}

	return err, buf

}

func (e *executor) commit(buf []*Response, err error, chn chan<- *Response) (error, []*Response) {

	defer e.dbo.Reset()

	if e.dbo.TX == nil {
		return nil, buf
	}

	if err != nil {
		err = e.dbo.Cancel()
	} else {
		err = e.dbo.Commit()
	}

	for _, v := range buf {
		if err != nil {
			v.Status = "ERR"
			v.Result = []interface{}{}
			v.Detail = "Transaction failed: " + err.Error()
		}
		chn <- v
	}

	for i := len(buf) - 1; i >= 0; i-- {
		buf[len(buf)-1] = nil
		buf = buf[:len(buf)-1]
	}

	return err, buf

}

func status(e error) (s string) {
	switch e.(type) {
	default:
		return "OK"
	case *kvs.DBError:
		return "ERR_DB"
	case *kvs.KVError:
		return "ERR_KV"
	case *PermsError:
		return "ERR_PE"
	case *ExistError:
		return "ERR_EX"
	case *FieldError:
		return "ERR_FD"
	case *IndexError:
		return "ERR_IX"
	case *TimerError:
		return "ERR_TO"
	case error:
		return "ERR"
	}
}

func detail(e error) (s string) {
	switch err := e.(type) {
	default:
		return
	case error:
		return err.Error()
	}
}

func groupd(buf []*Response, rsp *Response) []*Response {
	for i := len(buf) - 1; i >= 0; i-- {
		buf[len(buf)-1] = nil
		buf = buf[:len(buf)-1]
	}
	return append(buf, rsp)
}
