// The GraphQL server the e2e workflows run probe-graphql against. Its
// queries are run by graphql-go, an implementation the action was not
// written against, over a schema small enough to read here.
//
//	go -C e2e/server run . -addr 127.0.0.1:18081
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/graphql-go/graphql"
)

type country struct {
	Code    string  `json:"code"`
	Name    string  `json:"name"`
	Capital *string `json:"capital"`
}

// store holds the countries, those it starts with and those a mutation adds.
type store struct {
	mu        sync.Mutex
	countries []country
}

func (s *store) find(code string) *country {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.countries {
		if c.Code == code {
			return &c
		}
	}
	return nil
}

func (s *store) add(c country) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, have := range s.countries {
		if have.Code == c.Code {
			return fmt.Errorf("the country %s is already there", c.Code)
		}
	}
	s.countries = append(s.countries, c)
	return nil
}

func (s *store) all() []country {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]country(nil), s.countries...)
}

func ptr(s string) *string { return &s }

// headerKey is the key the headers of a request are kept under in the
// context of its query.
type headerKey struct{}

func newSchema(s *store) (graphql.Schema, error) {
	countryType := graphql.NewObject(graphql.ObjectConfig{
		Name: "Country",
		Fields: graphql.Fields{
			"code":    &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"name":    &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"capital": &graphql.Field{Type: graphql.String},
		},
	})
	header := func(p graphql.ResolveParams, name string) string {
		h, _ := p.Context.Value(headerKey{}).(http.Header)
		return h.Get(name)
	}
	query := graphql.NewObject(graphql.ObjectConfig{
		Name: "Query",
		Fields: graphql.Fields{
			"country": &graphql.Field{
				Type: countryType,
				Args: graphql.FieldConfigArgument{
					"code": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.ID)},
				},
				Resolve: func(p graphql.ResolveParams) (any, error) {
					if c := s.find(p.Args["code"].(string)); c != nil {
						return *c, nil
					}
					return nil, nil
				},
			},
			"countries": &graphql.Field{
				Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(countryType))),
				Resolve: func(p graphql.ResolveParams) (any, error) {
					return s.all(), nil
				},
			},
			// viewer is who the request says it is from, and an error of
			// its field when it does not say.
			"viewer": &graphql.Field{
				Type: graphql.String,
				Resolve: func(p graphql.ResolveParams) (any, error) {
					token, ok := strings.CutPrefix(header(p, "Authorization"), "Bearer ")
					if !ok {
						return nil, errors.New("not signed in")
					}
					return token, nil
				},
			},
			"userAgent": &graphql.Field{
				Type: graphql.String,
				Resolve: func(p graphql.ResolveParams) (any, error) {
					return header(p, "User-Agent"), nil
				},
			},
		},
	})
	mutation := graphql.NewObject(graphql.ObjectConfig{
		Name: "Mutation",
		Fields: graphql.Fields{
			"addCountry": &graphql.Field{
				Type: graphql.NewNonNull(countryType),
				Args: graphql.FieldConfigArgument{
					"code":    &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.ID)},
					"name":    &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
					"capital": &graphql.ArgumentConfig{Type: graphql.String},
				},
				Resolve: func(p graphql.ResolveParams) (any, error) {
					c := country{Code: p.Args["code"].(string), Name: p.Args["name"].(string)}
					if capital, ok := p.Args["capital"].(string); ok {
						c.Capital = &capital
					}
					if err := s.add(c); err != nil {
						return nil, err
					}
					return c, nil
				},
			},
		},
	})
	return graphql.NewSchema(graphql.SchemaConfig{Query: query, Mutation: mutation})
}

// graphqlHandler answers a GraphQL request sent as a POST with a JSON body.
// A document that cannot be run, which has no data, is answered with 400,
// and an error of a field with 200 and the data there is, as GraphQL over
// HTTP has it for application/graphql-response+json.
func graphqlHandler(schema graphql.Schema) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "send a POST", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Query         string         `json:"query"`
			Variables     map[string]any `json:"variables"`
			OperationName string         `json:"operationName"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "the body is not JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		result := graphql.Do(graphql.Params{
			Schema:         schema,
			RequestString:  req.Query,
			VariableValues: req.Variables,
			OperationName:  req.OperationName,
			Context:        context.WithValue(r.Context(), headerKey{}, r.Header),
		})

		contentType := "application/json"
		if strings.Contains(r.Header.Get("Accept"), "application/graphql-response+json") {
			contentType = "application/graphql-response+json"
		}
		w.Header().Set("Content-Type", contentType+"; charset=utf-8")
		if result.Data == nil && result.HasErrors() {
			w.WriteHeader(http.StatusBadRequest)
		}
		_ = json.NewEncoder(w).Encode(result)
	}
}

func main() {
	addr := flag.String("addr", "127.0.0.1:18081", "the address to listen on")
	flag.Parse()

	schema, err := newSchema(&store{countries: []country{
		{Code: "JP", Name: "Japan", Capital: ptr("Tokyo")},
		{Code: "FR", Name: "France", Capital: ptr("Paris")},
		{Code: "AQ", Name: "Antarctica"},
	}})
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", graphqlHandler(schema))
	// A redirect that keeps the method and the body, to the endpoint.
	mux.HandleFunc("/moved", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/graphql", http.StatusTemporaryRedirect)
	})
	// What a proxy in front of a server that is down answers with.
	mux.HandleFunc("/down", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no server to answer", http.StatusBadGateway)
	})

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("listening on %s", ln.Addr())
	log.Fatal(http.Serve(ln, mux))
}
