package kusto

// conn.go holds the connection to the Kusto server and provides methods to do queries
// and receive Kusto frames back.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/Azure/azure-kusto-go/kusto/data/errors"
	"github.com/Azure/azure-kusto-go/kusto/internal/frames"
	v1 "github.com/Azure/azure-kusto-go/kusto/internal/frames/v1"
	v2 "github.com/Azure/azure-kusto-go/kusto/internal/frames/v2"
	"github.com/Azure/azure-kusto-go/kusto/internal/response"
	truestedEndpoints "github.com/Azure/azure-kusto-go/kusto/trusted_endpoints"
	"github.com/google/uuid"
)

var validURL = regexp.MustCompile(`https://([a-zA-Z0-9_-]+\.){1,2}.*`)

var bufferPool = sync.Pool{
	New: func() interface{} {
		return &bytes.Buffer{}
	},
}

// conn provides connectivity to a Kusto instance.
type conn struct {
	endpoint                       string
	auth                           Authorization
	endMgmt, endQuery, streamQuery *url.URL
	client                         *http.Client
	endpointValidated              atomic.Bool
	clientDetails                  *ClientDetails
}

// newConn returns a new conn object with an injected http.Client
func newConn(endpoint string, auth Authorization, client *http.Client, clientDetails *ClientDetails) (*conn, error) {
	if !validURL.MatchString(endpoint) {
		return nil, errors.ES(errors.OpServConn, errors.KClientArgs, "endpoint is not valid(%s), should be https://<cluster name>.*", endpoint).SetNoRetry()
	}

	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, errors.ES(errors.OpServConn, errors.KClientArgs, "could not parse the endpoint(%s): %s", endpoint, err).SetNoRetry()
	}

	c := &conn{
		auth:          auth,
		endMgmt:       &url.URL{Scheme: "https", Host: u.Host, Path: "/v1/rest/mgmt"},
		endQuery:      &url.URL{Scheme: "https", Host: u.Host, Path: "/v2/rest/query"},
		streamQuery:   &url.URL{Scheme: "https", Host: u.Host, Path: "/v1/rest/ingest/"},
		client:        client,
		clientDetails: clientDetails,
	}

	return c, nil
}

type queryMsg struct {
	DB         string            `json:"db"`
	CSL        string            `json:"csl"`
	Properties requestProperties `json:"properties,omitempty"`
}

type connOptions struct {
	queryOptions *queryOptions
	mgmtOptions  *mgmtOptions
}

// query makes a query for the purpose of extracting data from Kusto. Context can be used to set
// a timeout or cancel the query. Queries cannot take longer than 5 minutes.
func (c *conn) query(ctx context.Context, db string, query Stmt, options *queryOptions) (execResp, error) {
	if strings.HasPrefix(strings.TrimSpace(query.String()), ".") {
		return execResp{}, errors.ES(errors.OpQuery, errors.KClientArgs, "a Stmt to Query() cannot begin with a period(.), only Mgmt() calls can do that").SetNoRetry()
	}

	return c.execute(ctx, execQuery, db, query, *options.requestProperties)
}

// mgmt is used to do management queries to Kusto.
func (c *conn) mgmt(ctx context.Context, db string, query Stmt, options *mgmtOptions) (execResp, error) {
	return c.execute(ctx, execMgmt, db, query, *options.requestProperties)
}

func (c *conn) queryToJson(ctx context.Context, db string, query Stmt, options *queryOptions) (string, error) {
	_, _, _, body, e := c.doRequest(ctx, execQuery, db, query, *options.requestProperties)
	if e != nil {
		return "", e
	}

	defer body.Close()
	all, e := io.ReadAll(body)
	return string(all), e
}

const (
	execQuery = 1
	execMgmt  = 2
)

type execResp struct {
	reqHeader  http.Header
	respHeader http.Header
	frameCh    chan frames.Frame
}

func (c *conn) execute(ctx context.Context, execType int, db string, query Stmt, properties requestProperties) (execResp, error) {
	op, reqHeader, respHeader, body, e := c.doRequest(ctx, execType, db, query, properties)
	if e != nil {
		return execResp{}, e
	}

	var dec frames.Decoder
	switch execType {
	case execMgmt:
		dec = &v1.Decoder{}
	case execQuery:
		dec = &v2.Decoder{}
	default:
		return execResp{}, errors.ES(op, errors.KInternal, "unknown execution type was %v", execType).SetNoRetry()
	}

	frameCh := dec.Decode(ctx, body, op)

	return execResp{reqHeader: reqHeader, respHeader: respHeader, frameCh: frameCh}, nil
}

func (c *conn) doRequest(ctx context.Context, execType int, db string, query Stmt, properties requestProperties) (errors.Op, http.Header, http.Header,
	io.ReadCloser, error) {
	err := c.validateEndpoint()
	var op errors.Op
	if execType == execQuery {
		op = errors.OpQuery
	} else if execType == execMgmt {
		op = errors.OpMgmt
	}

	header := c.getHeaders(properties)

	var endpoint *url.URL
	buff := bufferPool.Get().(*bytes.Buffer)
	buff.Reset()
	defer bufferPool.Put(buff)

	switch execType {
	case execQuery, execMgmt:
		err = json.NewEncoder(buff).Encode(
			queryMsg{
				DB:         db,
				CSL:        query.String(),
				Properties: properties,
			},
		)
		if err != nil {
			return 0, nil, nil, nil, errors.E(op, errors.KInternal, fmt.Errorf("could not JSON marshal the Query message: %w", err))
		}
		if execType == execQuery {
			endpoint = c.endQuery
		} else {
			endpoint = c.endMgmt
		}
	default:
		return 0, nil, nil, nil, errors.ES(op, errors.KInternal, "internal error: did not understand the type of execType: %d", execType)
	}

	if c.auth.TokenProvider != nil && c.auth.TokenProvider.AuthorizationRequired() {
		c.auth.TokenProvider.SetHttp(c.client)
		token, tokenType, tkerr := c.auth.TokenProvider.AcquireToken(ctx)
		if tkerr != nil {
			return 0, nil, nil, nil, errors.ES(op, errors.KInternal, "Error while getting token : %s", tkerr)
		}
		header.Add("Authorization", fmt.Sprintf("%s %s", tokenType, token))
	}

	req := &http.Request{
		Method: http.MethodPost,
		URL:    endpoint,
		Header: header,
		Body:   io.NopCloser(buff),
	}

	resp, err := c.client.Do(req.WithContext(ctx))
	if err != nil {
		// TODO(jdoak): We need a http error unwrap function that pulls out an *errors.Error.
		return 0, nil, nil, nil, errors.E(op, errors.KHTTPError, fmt.Errorf("with query %q: %w", query.String(), err))
	}

	body, err := response.TranslateBody(resp, op)
	if err != nil {
		return 0, nil, nil, nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return 0, nil, nil, nil, errors.HTTP(op, resp.Status, resp.StatusCode, body, fmt.Sprintf("error from Kusto endpoint for query %q: ", query.String()))
	}
	return op, header, resp.Header, body, nil
}

func (c *conn) validateEndpoint() error {
	if !c.endpointValidated.Load() {
		var err error
		if cloud, err := GetMetadata(c.endpoint, c.client); err == nil {
			err = truestedEndpoints.Instance.ValidateTrustedEndpoint(c.endpoint, cloud.LoginEndpoint)
			if err == nil {
				c.endpointValidated.Store(true)
			}
		}

		return err
	}

	return nil
}

func (c *conn) getHeaders(properties requestProperties) http.Header {
	header := http.Header{}
	header.Add("Accept", "application/json")
	header.Add("Accept-Encoding", "gzip")
	header.Add("Content-Type", "application/json; charset=utf-8")
	header.Add("x-ms-version", "2019-02-13")

	if properties.ClientRequestID != "" {
		header.Add("x-ms-client-request-id", properties.ClientRequestID)
	} else {
		header.Add("x-ms-client-request-id", "KGC.execute;"+uuid.New().String())
	}

	if properties.Application != "" {
		header.Add("x-ms-app", properties.Application)
	} else {
		header.Add("x-ms-app", c.clientDetails.ApplicationForTracing())
	}

	if properties.User != "" {
		header.Add("x-ms-user", properties.User)
	} else {
		header.Add("x-ms-user", c.clientDetails.UserNameForTracing())
	}

	header.Add("x-ms-client-version", c.clientDetails.ClientVersionForTracing())
	return header
}

func (c *conn) Close() error {
	c.client.CloseIdleConnections()
	return nil
}
