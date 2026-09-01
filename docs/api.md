# API Guide

This guide summarizes the API's durable semantics. The embedded
[OpenAPI document](../internal/apidocs/openapi.json) is the exhaustive HTTP
contract and is served at `/openapi.json`; interactive documentation is at
`/docs/`.

## Access and routes

All `/v1` routes require `Authorization: Bearer <token>`. The read-only token
permits reads and queries; the read/write token also permits replacement and
deletion. `/health`, `/openapi.json`, and `/docs/` are public.

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/v1/providers` | List providers |
| `GET` | `/v1/providers/{provider}/datasets` | List dataset summaries |
| `GET` | `/v1/providers/{provider}/datasets/{dataset}` | Get a summary |
| `POST` | `/v1/providers/{provider}/datasets/{dataset}` | Create or replace |
| `DELETE` | `/v1/providers/{provider}/datasets/{dataset}` | Idempotently delete |
| `GET` | `.../{dataset}/structure` | Get dimensions and categories |
| `GET` | `.../{dataset}/data` | Get the complete cube |
| `POST` | `.../{dataset}/query` | Get an exact Cartesian subset |

## Dataset payloads

Payloads borrow JSON-stat's local indexing and row-major ordering, but are not
complete JSON-stat documents. `id` defines dimension order, each `index` map
assigns contiguous zero-based positions, and the last dimension varies fastest.

```json
{
  "source_stamp": {"etag": "abc"},
  "id": ["sex", "year"],
  "dimension": {
    "sex":  {"index": {"M": 0, "F": 1}},
    "year": {"index": {"2024": 0, "2025": 1}}
  },
  "value":  [10.5, null, null, null],
  "text":   [null, null, null, "confidential"],
  "status": {"3": "c"}
}
```

`source_stamp` is required and may contain any JSON value, including `null`.
`replace` defaults to `false`; an existing dataset then returns `409`. Setting
`replace: true` creates or unconditionally replaces the dataset.

### Observation channels

- `value` is required and contains finite numbers or nulls.
- `text` is optional and contains strings or nulls. It uses the same dense or
  sparse representation as `value`.
- `status` is optional and independent of the value representation. It may be a
  full-length array, a sparse index object, or one string applied to every cell.
- A cell may have a numeric value or text, never both; status may accompany
  either or exist on a null-valued cell.

Dense channels are full-length arrays. Sparse channels are objects keyed by
canonical flattened indexes; omitted keys mean null, so explicit sparse nulls
are invalid. Empty optional channels are omitted from responses.

## Reads and queries

Full-data and query routes accept `?format=dense|sparse`; sparse is the default.
Full reads return dimensions and categories in normalized-code order. Query
requests must select at least one category from every dimension, and the
request's dimension and category indexes define response order.

Responses preserve stored code spellings and include complete-dataset counts,
even for a subset. Missing physical observations are reconstructed as nulls.

Codes match after bounded, repeated Unicode whitespace trimming, NFKC
normalization, and case folding. Empty normalized codes and normalized
duplicates are rejected.

## HTTP behavior

JSON body routes require `application/json`. Unknown fields, duplicate keys,
trailing JSON, invalid indexes, oversized cubes, and numeric/text collisions are
rejected. Errors use a stable envelope:

```json
{"error":{"code":"validation_failed","message":"...","request_id":"..."}}
```

Every response includes `X-Request-ID` and `Cache-Control: no-store`. Consult
OpenAPI for schemas, status codes, limits, and examples.
