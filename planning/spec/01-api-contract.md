# API Contract

## Contract Summary

This contract defines a private JSON API under `/v1`. Provider and dataset identities are hierarchical. Dataset creation and replacement are synchronous and atomic. Observation payloads borrow only JSON-stat's payload-local indexing concepts; they are not JSON-stat documents and contain no `version`, `class`, `size`, or `category` wrapper.

## Routes and Authorization

| Method | Route | Purpose | Read-only token |
|---|---|---|---|
| `GET` | `/health` | Database-aware readiness check | Public |
| `GET` | `/v1/providers` | List providers and dataset counts | Allowed |
| `GET` | `/v1/providers/{provider}/datasets` | List one provider's dataset summaries | Allowed |
| `GET` | `/v1/providers/{provider}/datasets/{dataset}` | Get one dataset summary | Allowed |
| `POST` | `/v1/providers/{provider}/datasets/{dataset}` | Create or completely replace a dataset | Forbidden |
| `DELETE` | `/v1/providers/{provider}/datasets/{dataset}` | Idempotently delete a dataset | Forbidden |
| `GET` | `/v1/providers/{provider}/datasets/{dataset}/structure` | Get all dimension/category codes | Allowed |
| `GET` | `/v1/providers/{provider}/datasets/{dataset}/data` | Get the complete observation cube | Allowed |
| `POST` | `/v1/providers/{provider}/datasets/{dataset}/query` | Get an exact Cartesian subset | Allowed |
| `GET` | `/openapi.json` | Get the OpenAPI 3.1 contract | Public |
| `GET` | `/docs` and `/docs/` | Redirect to and serve interactive API documentation | Public |

- Authentication uses `Authorization: Bearer <token>`.
- Missing or invalid credentials return `401`; a read-only token used for mutation returns `403`.
- Providers are created implicitly with their first dataset and listed only while they have datasets.
- Listings are unpaginated and sorted deterministically by normalized code.
- An unknown provider or dataset returns `404`.
- DELETE always returns `204`, including when the dataset is already absent.
- `/health`, `/openapi.json`, and `/docs` are the only public routes. Documentation exposes the contract, never stored data or credentials.

List responses use these envelopes:

```json
{"providers":[{"provider_code":"SCB","dataset_count":2}]}
```

```json
{"provider_code":"SCB","datasets":[{"provider_code":"SCB","dataset_code":"Population","source_stamp":null,"cell_count":4,"valued_cell_count":3,"null_cell_count":1,"updated_at":"2026-09-01T12:00:00Z"}]}
```

The single-dataset summary route returns the summary object directly. Creation and replacement return `{"result":"created|replaced","dataset":<summary>}`.

## Dataset Metadata

Dataset summaries and every structure, data, and query response contain:

```json
{
  "provider_code": "SCB",
  "dataset_code": "Population",
  "source_stamp": {"etag": "abc"},
  "cell_count": 4,
  "valued_cell_count": 3,
  "null_cell_count": 1,
  "updated_at": "2026-09-01T12:00:00Z"
}
```

- Counts always describe the complete stored dataset, including in subset responses.
- `valued_cell_count` counts cells containing either a numeric or text value.
- `null_cell_count` is `cell_count - valued_cell_count`; status-only cells remain null-valued.
- `updated_at` is the server-recorded time of the last successful creation or replacement.
- `source_stamp` is required on every creation or replacement and may contain any JSON value, including null. It is opaque and returned semantically, not byte-for-byte.

## Structure and Observation Encoding

A structure uses only `id` and `dimension`:

```json
{
  "id": ["sex", "year"],
  "dimension": {
    "sex": {"index": {"M": 0, "F": 1}},
    "year": {"index": {"2024": 0, "2025": 1}}
  }
}
```

- `id` defines payload-local dimension order.
- Every `id` entry occurs exactly once in `dimension`.
- Category indexes are unique, zero-based, and contiguous within each dimension.
- Indexes have no relationship to internal database indexes or business presentation order.
- Flattening is row-major: the last dimension varies fastest.
- Full structure and data responses sort dimensions and categories by normalized code before assigning indexes.
- Query responses preserve the requested dimension and category index order while returning stored code spellings.
- Query requests must include every dataset dimension exactly once and at least one existing category per dimension.
- Replacement structures contain between 1 and 64 dimensions, and every dimension contains at least one category.
- Submitted code spellings and their normalized keys are each limited to 256 UTF-8 bytes.

Observation channels are:

- Required `value`: numeric values and nulls.
- Optional `text`: string values and nulls.
- Optional `status`: strings and nulls, or one string applying to every logical cell.

`value`, `text`, and non-string `status` use one common representation:

- Dense: arrays whose lengths equal the dimension product.
- Sparse: objects keyed by canonical, base-10 flattened indexes; omitted keys mean null.

Numeric and text values are mutually exclusive at each index. Status is independent. `value` remains required for text-only datasets and may be an empty sparse object or an all-null dense array. Empty `text` and `status` channels are omitted from responses.

`value` selects the representation for the complete request. `text` and non-scalar `status`, when present, must use the same representation. A scalar request status expands to every payload-local logical cell. A response uses scalar status only when every returned cell has the same non-null status; otherwise it uses the requested dense/sparse representation and is omitted when all statuses are null.

Reads accept `?format=dense|sparse`; sparse is the default. Replacement requests may use either representation, inferred from `value`.

Every observation response combines metadata, selected structure, and channels:

```json
{
  "provider_code": "SCB",
  "dataset_code": "Population",
  "source_stamp": {"etag": "abc"},
  "cell_count": 4,
  "valued_cell_count": 2,
  "null_cell_count": 2,
  "updated_at": "2026-09-01T12:00:00Z",
  "id": ["sex", "year"],
  "dimension": {
    "sex": {"index": {"M": 0, "F": 1}},
    "year": {"index": {"2024": 0, "2025": 1}}
  },
  "value": {"0": 10.5},
  "text": {"3": "confidential"},
  "status": {"3": "c"}
}
```

A subset response includes every requested category, even when all corresponding cells are null.

Query requests contain only a payload-local structure selector:

```json
{
  "id": ["year", "sex"],
  "dimension": {
    "year": {"index": {"2025": 0}},
    "sex": {"index": {"F": 0, "M": 1}}
  }
}
```

The dimension and category indexes are unique, zero-based, contiguous, and define response order. Stored spellings replace request spellings in the response.

## Creation and Replacement

`POST /v1/providers/{provider}/datasets/{dataset}` accepts:

```json
{
  "replace": false,
  "source_stamp": {"etag": "abc"},
  "id": ["sex", "year"],
  "dimension": {
    "sex": {"index": {"M": 0, "F": 1}},
    "year": {"index": {"2024": 0, "2025": 1}}
  },
  "value": [10.5, null, null, null],
  "text": [null, null, null, "confidential"],
  "status": [null, null, null, "c"]
}
```

- `replace` defaults to `false`.
- When the dataset exists and `replace` is false, return `409 dataset_exists`.
- `replace: true` permits overwrite but also creates the dataset when absent.
- Creation returns `201`; replacement returns `200`.
- The response contains `result: "created"` or `"replaced"` and the resulting summary under `dataset`.
- Replacements are unconditional. Same-dataset writes serialize, and the last successful request wins.
- A failure leaves the previous dataset fully visible and unchanged.

## Validation and Identity

- Codes match after trimming surrounding Unicode whitespace, Unicode NFKC normalization, and Unicode case folding.
- Empty normalized codes and normalized duplicates are invalid.
- Provider and dataset response spelling is preserved from first creation; each replacement supplies the current stored spelling for dimensions and categories.
- Retained provider spelling survives deletion of its last dataset. A deleted and later recreated dataset establishes a new first-creation spelling.
- Provider and dataset path codes cannot contain `/`.
- The logical dimension product must be between 1 and 1,000,000 without overflow.
- Dense channels must have exactly the logical cell count.
- Sparse keys must be canonical non-negative integers within the cube.
- Numeric entries must fit finite `float64`; NaN and infinities are invalid and must be mapped to null upstream.
- Sparse channels cannot contain explicit null entries; omission represents null.
- Unknown fields, duplicate JSON object keys, trailing JSON values, invalid channel types, and numeric/text collisions are rejected.
- JSON body routes require `Content-Type: application/json`.
- `application/json` may include no charset or `charset=utf-8`. Other media types, charsets, and content encodings are unsupported.
- Oversized bodies return `413` before database work.
- Only full-data and query reads accept a query parameter: exactly one `format=dense|sparse`. Unknown or duplicate parameters are invalid.

## Errors and HTTP Behavior

All errors use:

```json
{
  "error": {
    "code": "validation_failed",
    "message": "dimension indexes must be contiguous",
    "request_id": "..."
  }
}
```

- `400`: malformed JSON or invalid query syntax.
- `401`: missing or invalid bearer token.
- `403`: valid read-only token used for mutation.
- `404`: unknown route, provider, or dataset.
- `405`: unsupported method, with `Allow`.
- `409`: create-only POST conflicts with an existing dataset.
- `413`: request body exceeds the configured limit.
- `415`: unsupported media type.
- `422`: syntactically valid input violates dataset rules.
- `500`: unexpected internal failure.
- `503`: database unavailable or operation deadline exceeded.

Stable error codes are `invalid_json`, `invalid_query`, `invalid_path_code`, `unauthorized`, `forbidden`, `not_found`, `method_not_allowed`, `dataset_exists`, `request_too_large`, `unsupported_media_type`, `validation_failed`, `cell_limit_exceeded`, `internal_error`, and `service_unavailable`. Validation messages may become more precise without changing the code.

Every response includes `X-Request-ID`; JSON responses use UTF-8 and `Cache-Control: no-store`. `/health` returns `200 {"status":"ok"}` or `503 {"status":"unavailable"}` without internal database details.

## Contract Acceptance Scenarios

- Create, list, summarize, read, query, replace, and idempotently delete a dataset.
- Reject accidental overwrite unless `replace: true`.
- Enforce read-only versus read/write credentials.
- Round-trip dense and sparse numeric, text, status-only, and inferred-null cells.
- Verify row-major indexes and requested query ordering.
- Verify normalized lookup, duplicate-code rejection, and canonical response spelling.
- Verify full-dataset counts remain present in subset responses.
- Verify failed and concurrent replacements never expose partial state.
- Verify fully populated one-million-cell requests and responses.
