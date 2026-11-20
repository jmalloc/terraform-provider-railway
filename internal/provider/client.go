package provider

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httputil"
	"os"

	"github.com/Khan/genqlient/graphql"
)

type railwayClients struct {
	api           graphql.Client
	projectTokens graphql.Client
}

// Token operations can return credentials in response bodies. Use a client
// with HTTP dumps disabled and keep credentials off redirect requests.
func newProjectTokenClient(token string, projectScoped bool) graphql.Client {
	return graphql.NewClient(railwayEndpoint, &http.Client{
		Transport: &authedTransport{
			token:        token,
			projectToken: projectScoped,
			wrapped:      http.DefaultTransport,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	})
}

type authedTransport struct {
	token        string
	projectToken bool
	debugHTTP    bool
	wrapped      http.RoundTripper
}

func (t *authedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	if t.projectToken {
		req.Header.Set("Project-Access-Token", t.token)
	} else {
		req.Header.Set("Authorization", "Bearer "+t.token)
	}

	debug := t.debugHTTP && os.Getenv("HTTP_DEBUG") == "1"

	if debug {
		dump, err := httputil.DumpRequestOut(req, true)

		if err != nil {
			fmt.Fprintf(os.Stderr, "Unable to dump Railway request: %v\n", err)
		} else {
			dump = bytes.ReplaceAll(
				dump,
				[]byte("Bearer "+t.token),
				[]byte("Bearer [REDACTED]"),
			)

			fmt.Fprintf(os.Stderr, "\nRailway request:\n%s\n", dump)
		}
	}

	resp, err := t.wrapped.RoundTrip(req)

	if debug && resp != nil {
		dump, dumpErr := httputil.DumpResponse(resp, true)

		if dumpErr != nil {
			fmt.Fprintf(os.Stderr, "Unable to dump Railway response: %v\n", dumpErr)
		} else {
			fmt.Fprintf(os.Stderr, "\nRailway response:\n%s\n", dump)
		}
	}

	return resp, err
}
