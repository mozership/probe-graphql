# probe-graphql

A [Probe](https://github.com/mozership/probe) action that sends a GraphQL query over HTTP.

Probe downloads it the first time a workflow uses it. It needs Probe v1.17.0 or later, and v1.21.0 or later to run under a guard, which reads the guard and the params the action declares in `action.yml`.

```yaml
name: Countries API
actions:
  graphql: github.com/mozership/probe-graphql@<commit SHA>
jobs:
- name: countries
  steps:
  - name: Look up Japan
    uses: graphql
    with:
      url: https://countries.trevorblades.com/graphql
      query: |
        query Country($code: ID!) {
          country(code: $code) { name capital currency }
        }
      variables:
        code: JP
    test: res.code == 200 && len(res.errors) == 0 && res.data.country.capital == "Tokyo"
```

The workflow gives the action a name once, under `actions`, and its steps use the name. Probe only takes a full 40-character commit SHA there. The notes of each [release](https://github.com/mozership/probe-graphql/releases) start with the action and its commit to copy.

`actions` needs Probe v1.24.0 or later. With an earlier one, write the action in full in each `uses`, as `uses: github.com/mozership/probe-graphql@<commit SHA>`.

## Parameters

| Parameter | Type | Required | Default | Description |
|---|---|---|---|---|
| `url` | String | Yes | - | GraphQL endpoint, `http` or `https` |
| `query` | String | Yes | - | The query or mutation document |
| `variables` | Object | No | - | Values for the variables the document declares |
| `operation_name` | String | No | - | The operation to run when the document has more than one |
| `headers` | Object | No | - | Request headers, such as `Authorization`. They override the defaults below |
| `timeout` | Duration | No | `30s` | Time limit for the request, as `10s` or a number of seconds. `0` removes it |

A key of `with` that is not one of these fails the step before anything is sent, and `probe check` reports it with its line.

The request is a `POST` with a JSON body, sent with `Content-Type: application/json`, `Accept: application/graphql-response+json, application/json` and `User-Agent: probe-graphql/<version>`.

## Result

| Field | Type | Description |
|---|---|---|
| `res.code` | Integer | HTTP status code |
| `res.status` | String | HTTP status line, such as `"200 OK"` |
| `res.headers` | Object | Response headers, keyed by canonical name |
| `res.data` | Any | The `data` of the response, or `null` |
| `res.errors` | Array | The `errors` of the response; empty when there are none |
| `res.body` | Any | The whole response body, parsed when it is JSON, otherwise the raw string |
| `res.rawbody` | String | The unparsed body, present when the body is JSON |
| `req` | Object | The `url`, `query`, `variables`, `operation_name` and `headers` that were sent |
| `rt` | Duration | Round-trip time |
| `status` | Integer | `0` when the status code is 2xx, the body is JSON and `errors` is empty; `1` otherwise |

Any response the server sends is a result, so a test can assert on a GraphQL error or a 500. Only a request that gets no response, such as a refused connection or a timeout, fails the step as an error.

## Guard

The action keeps to the guard of the run, and its `action.yml` declares `guard: [read-only, allow-host]`, so Probe runs it under `--read-only` and `--allow-host` without `--allow-action`.

- Under `--read-only`, only a query is sent. The operation to run, the one `operation_name` names or the only one in the document, is read with a GraphQL parser, and a mutation or a subscription is refused before anything is sent. So is a document that does not parse, one with several operations and no `operation_name`, and an `operation_name` the document does not have.
- Under `--allow-host`, the host of `url`, and of each redirect, must be one the run allows. A URL without a port is taken at the port of its scheme.

A refused step fails with the kind `refused`.

## Testing

`go test ./...` runs against a stub server. `e2e/workflow.yml` runs the action through Probe against the GraphQL server of `e2e/server`, whose queries are run by [graphql-go](https://github.com/graphql-go/graphql):

```sh
go -C e2e/server run . &
go build -o e2e/action/probe-graphql . && probe e2e/workflow.yml
```

`GRAPHQL_URL` names the server when it listens elsewhere than on `127.0.0.1:18081`. `e2e/guard.yml` is run under `--read-only` and under `--allow-host`, where its steps are refused.

## Releasing

Pushing a `v*` tag builds the executables with GoReleaser and publishes them on the release. The workflow then has `probe manifest` write an `action.yml` with their URLs and SHA-256 digests, commits it to `main`, and adds the commit to the release notes. The name, the description, `guard` and `params` are those of the `action.yml` at the tag, so that file is where to change them. That commit is the one to pin: Probe reads `action.yml` at the pinned commit and refuses an executable whose digest differs.

## License

MIT
