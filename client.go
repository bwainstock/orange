package orange

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// defaultQueryLengthThreshold defines the maximum length of the URI for an
// outgoing GET query.  Queries that require a longer URI will automatically be
// sent out via a PUT query.
const defaultQueryLengthThreshold = 4096

// Client provides a Query method that resolves range queries.
type Client struct {
	// The only thing that prevents us from exposing a structure with all public
	// fields is the fact that we need to create the round robin list of
	// servers, and validate other config parameters.
	httpClient    Doer
	servers       *roundRobinStrings
	retryCallback func(error) bool
	retryCount    int
	retryPause    time.Duration
}

// NewClient returns a new instance that sends queries to one or more range
// servers.  The provided Config not only provides a way of listing one or more
// range servers, but also allows specification of optional retry-on-failure
// features.
//
//     func main() {
//         // Create a range client.  Programs can list more than one server and
//         // include other options.  See Config structure documentation for specifics.
//         client, err := orange.NewClient(&orange.Config{
//             Servers: []string{"localhost:8081"},
//         })
//         if err != nil {
//             fmt.Fprintf(os.Stderr, "%s\n", err)
//             os.Exit(1)
//         }
//
//         // Example program main loop reads query from standard input, queries the
//         // range server, then prints the response.
//         fmt.Printf("> ")
//         scanner := bufio.NewScanner(os.Stdin)
//         for scanner.Scan() {
//             values, err := client.Query(scanner.Text())
//             if err != nil {
//                 fmt.Fprintf(os.Stderr, "ERROR: %s\n", err)
//                 fmt.Printf("> ")
//                 continue
//             }
//             fmt.Printf("%v\n> ", values)
//         }
//         if err := scanner.Err(); err != nil {
//             fmt.Fprintf(os.Stderr, "ERROR: %s\n", err)
//         }
//     }
func NewClient(config *Config) (*Client, error) {
	if config.RetryCount < 0 {
		return nil, fmt.Errorf("cannot create Querier with negative RetryCount: %d", config.RetryCount)
	}
	if config.RetryPause < 0 {
		return nil, fmt.Errorf("cannot create Querier with negative RetryPause: %s", config.RetryPause)
	}
	rrs, err := newRoundRobinStrings(config.Servers)
	if err != nil {
		return nil, fmt.Errorf("cannot create Querier without at least one range server address")
	}

	retryCallback := config.RetryCallback
	if retryCallback == nil {
		retryCallback = makeRetryCallback(len(config.Servers))
	}

	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{
			// WARNING: Using http.Client instance without a Timeout will cause
			// resource leaks and may render your program inoperative if the
			// client connects to a buggy range server, or over a poor network
			// connection.
			Timeout: time.Duration(DefaultQueryTimeout),

			Transport: &http.Transport{
				Dial: (&net.Dialer{
					Timeout:   DefaultDialTimeout,
					KeepAlive: DefaultDialKeepAlive,
				}).Dial,
				MaxIdleConnsPerHost: int(DefaultMaxIdleConnsPerHost),
			},
		}
	}

	client := &Client{
		httpClient:    httpClient,
		retryCallback: retryCallback,
		retryCount:    config.RetryCount,
		retryPause:    config.RetryPause,
		servers:       rrs,
	}

	return client, nil
}

// Query sends out a query and returns either a slice of strings corresponding
// to the query response or an error.
//
// The query is sent to one or more of the configured range servers.  If a
// particular query results in an error, the query is retried according to the
// client's RetryCount setting.
//
// If a response includes a RangeException header, it returns ErrRangeException.
// If a query's response HTTP status code is not okay, it returns
// ErrStatusNotOK.
//
//     func main() {
//         // Create a range client.  Programs can list more than one server and
//         // include other options.  See Config structure documentation for specifics.
//         client, err := orange.NewClient(&orange.Config{
//             Servers: []string{"localhost:8081"},
//         })
//         if err != nil {
//             fmt.Fprintf(os.Stderr, "%s\n", err)
//             os.Exit(1)
//         }
//
//         // Example program main loop reads query from standard input, queries the
//         // range server, then prints the response.
//         fmt.Printf("> ")
//         scanner := bufio.NewScanner(os.Stdin)
//         for scanner.Scan() {
//             values, err := client.Query(scanner.Text())
//             if err != nil {
//                 fmt.Fprintf(os.Stderr, "ERROR: %s\n", err)
//                 fmt.Printf("> ")
//                 continue
//             }
//             fmt.Printf("%v\n> ", values)
//         }
//         if err := scanner.Err(); err != nil {
//             fmt.Fprintf(os.Stderr, "ERROR: %s\n", err)
//         }
//     }
func (c *Client) Query(expression string) ([]string, error) {
	r, err := c.query(context.Background(), expression)
	if err == nil {
		return r.Split(), nil
	}
	return nil, err
}

// QueryCtx sends the query expression to the range client with the provided
// query context.  Callers may opt to use this method when a timeout is required
// for the query.  Note that the shorter timeout applies when using a
// http.Client timeout and a context timeout.  If you intend to only use
// QueryCtx and QueryBytesCtx, then also you might want to pass a different
// HTTPClient argument to the Config so the two timeouts do not cause unexpected
// results.
//
//     func main() {
//         optTimeout := flag.Duration("timeout", 0, "timeout duration for the query")
//         flag.Parse()
//
//         // Create a range client.  Programs can list more than one server and
//         // include other options.  See Config structure documentation for specifics.
//         client, err := orange.NewClient(&orange.Config{
//             Servers: []string{"localhost:8081"},
//         })
//         if err != nil {
//             fmt.Fprintf(os.Stderr, "%s\n", err)
//             os.Exit(1)
//         }
//
//         ctx := context.Background()
//         if *optTimeout > 0 {
//             var done func()
//             ctx, done = context.WithTimeout(ctx, *optTimeout)
//             defer done()
//         }
//
//         if flag.NArg() == 0 {
//             fmt.Fprintf(os.Stderr, "USAGE: %s [-timeout DURATION] q1 q2\n")
//             os.Exit(1)
//         }
//
//         values, err := client.Query(strings.Join(flag.Args(), ","))
//         if err != nil {
//             fmt.Fprintf(os.Stderr, "ERROR: %s\n", err)
//             os.Exit(1)
//         }
//
//         fmt.Println(values)
//     }
func (c *Client) QueryCtx(ctx context.Context, expression string) ([]string, error) {
	r, err := c.query(ctx, expression)
	if err == nil {
		return r.Split(), nil
	}
	return nil, err
}

// QueryBytes sends out a query and returns either a slice of bytes
// corresponding to the HTTP response body received from the range server, or an
// error.
//
// The query is sent to one or more of the configured range servers.  If a
// particular query results in an error, the query is retried according to the
// client's RetryCount setting.
//
// If a response includes a RangeException header, it returns ErrRangeException.
// If a query's response HTTP status code is not okay, it returns
// ErrStatusNotOK.
//
//     func main() {
//         // Create a range client.  Programs can list more than one server and
//         // include other options.  See Config structure documentation for specifics.
//         client, err := orange.NewClient(&orange.Config{
//             Servers: []string{"localhost:8081"},
//         })
//         if err != nil {
//             fmt.Fprintf(os.Stderr, "%s\n", err)
//             os.Exit(1)
//         }
//
//         // Example program main loop reads query from standard input, queries the
//         // range server, then prints the response.
//         fmt.Printf("> ")
//         scanner := bufio.NewScanner(os.Stdin)
//         for scanner.Scan() {
//             buf, err := client.QueryBytes(scanner.Text())
//             if err != nil {
//                 fmt.Fprintf(os.Stderr, "ERROR: %s\n", err)
//                 fmt.Printf("> ")
//                 continue
//             }
//             fmt.Println(string(buf))
//         }
//         if err := scanner.Err(); err != nil {
//             fmt.Fprintf(os.Stderr, "ERROR: %s\n", err)
//         }
//     }
func (c *Client) QueryBytes(expression string) ([]byte, error) {
	r, err := c.query(context.Background(), expression)
	if err == nil {
		return r.Bytes(), nil
	}
	return nil, err
}

// QueryBytesCtx sends the query expression to the range client with the
// provided query context.  Callers may opt to use this method when a timeout is
// required for the query.  Note that the shorter timeout applies when using a
// http.Client timeout and a context timeout.  If you intend to only use
// QueryCtx and QueryBytesCtx, then you also might want to pass a different
// HTTPClient argument to the Config so the two timeouts do not cause unexpected
// results.
func (c *Client) QueryBytesCtx(ctx context.Context, expression string) ([]byte, error) {
	r, err := c.query(ctx, expression)
	if err == nil {
		return r.Bytes(), nil
	}
	return nil, err
}

func (c *Client) query(ctx context.Context, expression string) (*response, error) {
	type responseResult struct {
		r *response
		e error
	}

	ch := make(chan responseResult, 1)

	// Spawn a go-routine to send queries to one or more range servers, as
	// allowed by the client's Servers and Retry settings.
	go func() {
		var attempts int
		for {
			buf, err := c.getFromRangeServer(ctx, expression)
			if attempts == c.retryCount || err == nil || c.retryCallback(err) == false {
				if err == nil {
					ch <- responseResult{r: newResponse(buf)}
				} else {
					ch <- responseResult{e: err}
				}
				return
			}
			attempts++
			if c.retryPause > 0 {
				time.Sleep(c.retryPause)
			}
		}
	}()

	// Block and wait for either a response or the context to be closed by the
	// caller.
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case rr := <-ch:
		return rr.r, rr.e
	}
}

// getFromRangeServer sends to server the query and returns either a byte slice
// from reading the valid server response, or an error. This function attempts
// to send the query using both GET and PUT HTTP methods. It defaults to using
// GET first, then trying PUT, unless the query length is longer than a program
// constant, in which case it first tries PUT then will try GET.
func (c *Client) getFromRangeServer(ctx context.Context, expression string) ([]byte, error) {
	var err, herr error
	var response *http.Response

	// need endpoint for both GET and PUT, so keep it separate
	endpoint := fmt.Sprintf("http://%s/range/list", c.servers.Next())

	// need uri for just GET
	uri := fmt.Sprintf("%s?%s", endpoint, url.QueryEscape(expression))

	// Default to using GET request because most servers support it. However,
	// opt for PUT when extremely long query length.
	var method string
	if len(uri) > defaultQueryLengthThreshold {
		method = http.MethodPut
	} else {
		method = http.MethodGet
	}

	// At least 2 tries so we can try GET or POST if server gives us 405 or 414.
	for triesRemaining := 2; triesRemaining > 0; triesRemaining-- {
		select {
		case <-ctx.Done():
			return nil, ctx.Err() // terminate when client has canceled the context
		default:
			// context still valid: fallthrough and send out a query attempt
		}

		switch method {
		case http.MethodGet:
			response, err = c.getQuery(ctx, uri)
		case http.MethodPut:
			response, err = c.putQuery(ctx, endpoint, expression)
		default:
			panic(fmt.Errorf("cannot use unsupported HTTP method: %q", method))
		}
		if err != nil {
			// Could not make network request, or perhaps context closed by
			// caller while waiting for response.
			return nil, err
		}

		// Network round trip completed successfully, but there still might be
		// an error condition encoded in the response.

		switch response.StatusCode {
		case http.StatusOK:
			if message := response.Header.Get("RangeException"); message != "" {
				return nil, ErrRangeException{Message: message}
			}
			//
			// NORMAL EXIT PATH: range server provided non-error response
			//
			return readAndClose(response.Body)
		case http.StatusRequestURITooLong:
			method = http.MethodPut // try again using PUT
			herr = ErrStatusNotOK{
				Status:     response.Status,
				StatusCode: response.StatusCode,
			}
		case http.StatusMethodNotAllowed:
			method = http.MethodGet // try again using GET
			herr = ErrStatusNotOK{
				Status:     response.Status,
				StatusCode: response.StatusCode,
			}
		default:
			herr = ErrStatusNotOK{
				Status:     response.Status,
				StatusCode: response.StatusCode,
			}
		}
	}

	return nil, herr
}

func (c *Client) getQuery(ctx context.Context, url string) (*http.Response, error) {
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return c.httpClient.Do(request.WithContext(ctx))
}

func (c *Client) putQuery(ctx context.Context, endpoint, expression string) (*http.Response, error) {
	form := url.Values{"query": []string{expression}}
	request, err := http.NewRequest(http.MethodPut, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	request.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	return c.httpClient.Do(request.WithContext(ctx))
}

func readAndClose(rc io.ReadCloser) ([]byte, error) {
	buf, rerr := ioutil.ReadAll(rc)
	cerr := rc.Close() // always close regardless of read error
	if rerr != nil {
		return nil, rerr // Read error has more context than Close error
	}
	if cerr != nil {
		return nil, cerr
	}
	return buf, nil
}
