package kusto

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-kusto-go/kusto/data/errors"
	"github.com/Azure/azure-kusto-go/kusto/internal/frames"
	v2 "github.com/Azure/azure-kusto-go/kusto/internal/frames/v2"
)

// queryer provides for getting a stream of Kusto frames. Exists to allow fake Kusto streams in tests.
type queryer interface {
	io.Closer
	query(ctx context.Context, db string, query Stmt, options *queryOptions) (execResp, error)
	mgmt(ctx context.Context, db string, query Stmt, options *mgmtOptions) (execResp, error)
	queryToJson(ctx context.Context, db string, query Stmt, options *queryOptions) (string, error)
}

// Authorization provides the TokenProvider needed to acquire the auth token.
type Authorization struct {
	// Token provider that can be used to get the access token.
	TokenProvider *TokenProvider
}

// Client is a client to a Kusto instance.
type Client struct {
	conn, ingestConn queryer
	endpoint         string
	auth             Authorization
	mgmtConnMu       sync.Mutex
	http             *http.Client
	clientDetails    *ClientDetails
}

// Option is an optional argument type for New().
type Option func(c *Client)

// New returns a new Client.
func New(kcsb *ConnectionStringBuilder, options ...Option) (*Client, error) {
	tkp, err := kcsb.newTokenProvider()
	if err != nil {
		return nil, err
	}
	auth := &Authorization{
		TokenProvider: tkp,
	}
	endpoint := kcsb.DataSource
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, errors.ES(errors.OpServConn, errors.KClientArgs, "could not parse the endpoint(%s): %s", endpoint, err).SetNoRetry()
	}
	if strings.HasPrefix(u.Hostname(), "ingest-") {
		return nil, errors.ES(
			errors.OpServConn,
			errors.KClientArgs,
			"endpoint argument started with 'ingest-'. Adding 'ingest-' is taken care of by the client. "+
				"If using Mgmt() on an ingestion endpoint, use option QueryIngestion(). This is very uncommon",
		)
	}

	client := &Client{auth: *auth, endpoint: endpoint, clientDetails: NewClientDetails(kcsb.ApplicationForTracing, kcsb.UserForTracing)}
	for _, o := range options {
		o(client)
	}

	if client.http == nil {
		client.http = &http.Client{}
	}

	conn, err := newConn(endpoint, *auth, client.http, client.clientDetails)
	if err != nil {
		return nil, err
	}
	client.conn = conn

	return client, nil
}

func WithHttpClient(client *http.Client) Option {
	return func(c *Client) {
		c.http = client
	}
}

// QueryOption is an option type for a call to Query().
type QueryOption func(q *queryOptions) error

// Note: QueryOption are defined in queryopts.go file

// MgmtOption is an option type for a call to Mgmt().
type MgmtOption func(m *mgmtOptions) error

// Note: MgmtOption are defined in queryopts.go file

// Auth returns the Authorization passed to New().
func (c *Client) Auth() Authorization {
	return c.auth
}

// Endpoint returns the endpoint passed to New().
func (c *Client) Endpoint() string {
	return c.endpoint
}

type callType int8

const (
	unknownCallType = 0
	queryCall       = 1
	mgmtCall        = 2
)

// Query queries Kusto for data. context can set a timeout or cancel the query.
// query is a injection safe Stmt object. Queries cannot take longer than 5 minutes by default and have row/size limitations.
// Note that the server has a timeout of 4 minutes for a query by default unless the context deadline is set. Queries can
// take a maximum of 1 hour.
func (c *Client) Query(ctx context.Context, db string, query Stmt, options ...QueryOption) (*RowIterator, error) {
	ctx, cancel, err := contextSetup(ctx, false) // Note: cancel is called when *RowIterator has Stop() called.
	if err != nil {
		return nil, err
	}

	opts, err := setQueryOptions(ctx, errors.OpQuery, query, options...)
	if err != nil {
		return nil, err
	}

	conn, err := c.getConn(queryCall, connOptions{queryOptions: opts})
	if err != nil {
		return nil, err
	}

	execResp, err := conn.query(ctx, db, query, opts)
	if err != nil {
		cancel()
		return nil, err
	}

	var header v2.DataSetHeader

	ff := <-execResp.frameCh
	switch v := ff.(type) {
	case v2.DataSetHeader:
		header = v
	case frames.Error:
		cancel()
		return nil, v
	}

	iter, columnsReady := newRowIterator(ctx, cancel, execResp, header, errors.OpQuery)

	var sm stateMachine
	if header.IsProgressive {
		sm = &progressiveSM{
			op:   errors.OpQuery,
			iter: iter,
			in:   execResp.frameCh,
			ctx:  ctx,
			wg:   &sync.WaitGroup{},
		}
	} else {
		sm = &nonProgressiveSM{
			op:   errors.OpQuery,
			iter: iter,
			in:   execResp.frameCh,
			ctx:  ctx,
			wg:   &sync.WaitGroup{},
		}
	}
	go runSM(sm)

	<-columnsReady

	return iter, nil
}

func (c *Client) QueryToJson(ctx context.Context, db string, query Stmt, options ...QueryOption) (string, error) {
	ctx, cancel, err := contextSetup(ctx, false) // Note: cancel is called when *RowIterator has Stop() called.
	if err != nil {
		return "", err
	}

	opts, err := setQueryOptions(ctx, errors.OpQuery, query, options...)
	if err != nil {
		return "", err
	}

	conn, err := c.getConn(queryCall, connOptions{queryOptions: opts})
	if err != nil {
		return "", err
	}

	json, err := conn.queryToJson(ctx, db, query, opts)
	if err != nil {
		cancel()
		return "", err
	}

	return json, nil
}

// Mgmt is used to do management queries to Kusto.
// Details can be found at: https://docs.microsoft.com/en-us/azure/kusto/management/
// Mgmt accepts a Stmt, but that Stmt cannot have any query parameters attached at this time.
// Note that the server has a timeout of 10 minutes for a management call by default unless the context deadline is set.
// There is a maximum of 1 hour.
func (c *Client) Mgmt(ctx context.Context, db string, query Stmt, options ...MgmtOption) (*RowIterator, error) {

	if !query.params.IsZero() || !query.defs.IsZero() {
		return nil, errors.ES(errors.OpMgmt, errors.KClientArgs, "a Mgmt() call cannot accept a Stmt object that has Definitions or Parameters attached")
	}

	ctx, cancel, err := contextSetup(ctx, true) // Note: cancel is called when *RowIterator has Stop() called.
	if err != nil {
		return nil, err
	}

	opts, err := setMgmtOptions(ctx, errors.OpMgmt, query, options...)
	if err != nil {
		return nil, err
	}

	conn, err := c.getConn(mgmtCall, connOptions{mgmtOptions: opts})
	if err != nil {
		return nil, err
	}

	execResp, err := conn.mgmt(ctx, db, query, opts)
	if err != nil {
		cancel()
		return nil, err
	}

	iter, columnsReady := newRowIterator(ctx, cancel, execResp, v2.DataSetHeader{}, errors.OpMgmt)
	sm := &v1SM{
		op:   errors.OpQuery,
		iter: iter,
		in:   execResp.frameCh,
		ctx:  ctx,
		wg:   &sync.WaitGroup{},
	}

	go runSM(sm)

	<-columnsReady

	return iter, nil
}

func setQueryOptions(ctx context.Context, op errors.Op, query Stmt, options ...QueryOption) (*queryOptions, error) {
	params, err := query.params.toParameters(query.defs)
	if err != nil {
		return nil, errors.ES(op, errors.KClientArgs, "QueryValues in the the Stmt were incorrect: %s", err).SetNoRetry()
	}

	// Match our server deadline to our context.Deadline. This should be set from withing kusto.Query() to always have a value.
	deadline, ok := ctx.Deadline()
	if ok {
		options = append(
			options,
			queryServerTimeout(deadline.Sub(nower())),
		)
	}

	opt := &queryOptions{
		requestProperties: &requestProperties{
			Options:    map[string]interface{}{},
			Parameters: params,
		},
	}
	/*if op == errors.OpQuery {
		// We want progressive frames by default for Query(), but not Mgmt() because it uses v1 framing and ingestion endpoints
		// do not support it.
		opt.requestProperties.Options["results_progressive_enabled"] = true
	}*/
	opt.requestProperties.Options["results_progressive_enabled"] = true

	for _, o := range options {
		if err := o(opt); err != nil {
			return nil, errors.ES(op, errors.KClientArgs, "QueryValues in the the Stmt were incorrect: %s", err).SetNoRetry()
		}
	}
	return opt, nil
}

func setMgmtOptions(ctx context.Context, op errors.Op, query Stmt, options ...MgmtOption) (*mgmtOptions, error) {
	params, err := query.params.toParameters(query.defs)
	if err != nil {
		return nil, errors.ES(op, errors.KClientArgs, "QueryValues in the the Stmt were incorrect: %s", err).SetNoRetry()
	}

	// Match our server deadline to our context.Deadline. This should be set from withing kusto.Query() to always have a value.
	deadline, ok := ctx.Deadline()
	if ok {
		options = append(
			options,
			mgmtServerTimeout(deadline.Sub(nower())),
		)
	}

	opt := &mgmtOptions{
		requestProperties: &requestProperties{
			Options:    map[string]interface{}{},
			Parameters: params,
		},
	}
	if op == errors.OpQuery {
		// We want progressive frames by default for Query(), but not Mgmt() because it uses v1 framing and ingestion endpoints
		// do not support it.
		opt.requestProperties.Options["results_progressive_enabled"] = true
	}

	for _, o := range options {
		if err := o(opt); err != nil {
			return nil, errors.ES(op, errors.KClientArgs, "QueryValues in the the Stmt were incorrect: %s", err).SetNoRetry()
		}
	}
	return opt, nil
}

func (c *Client) getConn(callType callType, options connOptions) (queryer, error) {
	switch callType {
	case queryCall:
		return c.conn, nil
	case mgmtCall:
		delete(options.mgmtOptions.requestProperties.Options, "results_progressive_enabled")
		if options.mgmtOptions.queryIngestion {
			c.mgmtConnMu.Lock()
			defer c.mgmtConnMu.Unlock()

			if c.ingestConn != nil {
				return c.ingestConn, nil
			}

			u, _ := url.Parse(c.endpoint) // Don't care about the error
			u.Host = "ingest-" + u.Host
			auth := c.auth
			var details *ClientDetails
			if innerConn, ok := c.conn.(*conn); ok {
				details = innerConn.clientDetails
			}

			iconn, err := newConn(u.String(), auth, c.http, details)
			if err != nil {
				return nil, err
			}
			c.ingestConn = iconn

			return iconn, nil
		}
		return c.conn, nil
	default:
		return nil, errors.ES(errors.OpServConn, errors.KInternal, "an unknown calltype was passed to getConn()")
	}
}

var nower = time.Now

func contextSetup(ctx context.Context, mgmtCall bool) (context.Context, context.CancelFunc, error) {
	t, ok := ctx.Deadline()
	if ok {
		d := t.Sub(nower())
		if d > 1*time.Hour {
			if mgmtCall {
				return ctx, nil, errors.ES(errors.OpMgmt, errors.KClientArgs, "cannot set a deadline greater than 1 hour(%s)", d)
			}
			return ctx, nil, errors.ES(errors.OpQuery, errors.KClientArgs, "cannot set a deadline greater than 1 hour(%s)", d)
		}
		ctx, cancel := context.WithCancel(ctx)
		return ctx, cancel, nil
	}
	if mgmtCall {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		return ctx, cancel, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	return ctx, cancel, nil
}

func (c *Client) HttpClient() *http.Client {
	return c.http
}

func (c *Client) ClientDetails() *ClientDetails {
	return c.clientDetails
}

func (c *Client) Close() error {
	var err error
	if c.conn != nil {
		err = c.conn.Close()
	}
	if c.ingestConn != nil {
		err2 := c.ingestConn.Close()
		if err == nil {
			err = err2
		} else {
			err = errors.GetCombinedError(err, err2)
		}
	}
	return err
}
