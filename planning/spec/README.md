# Redesign Planning Specifications

These documents are the authoritative design for the clean-sheet v1 implementation. The legacy implementation was sealed in `archive/initial-implementation/` while the redesign was built and deleted once every acceptance gate passed; it was never a design input, and Git history is its only recovery mechanism.

## Reading Order and Authority

| Order | Document | Purpose | Status |
|---:|---|---|---|
| 1 | `00-goals-and-scope.md` | Product boundary and success criteria | Approved |
| 2 | `03-storage-semantics.md` | Identity, coordinate, transaction, and read rules | Approved |
| 3 | `01-api-contract.md` | Authoritative HTTP and JSON contract | Approved |
| 4 | `02-schema.sql` | Executable target PostgreSQL DDL | Approved |
| 5 | `04-operations.md` | Runtime, deployment, security, and observability | Approved |
| 6 | `05-test-and-acceptance.md` | Required verification and release gates | Approved |
| 7 | [`../../internal/apidocs/openapi.json`](../../internal/apidocs/openapi.json) | Machine-readable projection of the stable HTTP contract | Implemented and verified |

The more specialized document governs when statements overlap. Resolve a conflict by updating every affected document before changing code; do not silently choose one interpretation in the implementation.

The OpenAPI document is embedded in the API binary and served publicly at `/openapi.json`, with the interactive documentation at `/docs/`. It is a projection, not an authority: tests in `internal/apidocs` compare its operations against the registered route table, resolve every reference, and validate every example against its schema, so a contract change that is not mirrored there fails the build.

Design rationale stays beside the relevant rule. No legacy compatibility, data migration, or separate ADR set is required.
