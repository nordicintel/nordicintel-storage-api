# Redesign Planning Specifications

These documents are the authoritative design for the clean-sheet v1 implementation. The archived implementation and its documentation are not design inputs.

## Reading Order and Authority

| Order | Document | Purpose | Status |
|---:|---|---|---|
| 1 | `00-goals-and-scope.md` | Product boundary and success criteria | Approved |
| 2 | `03-storage-semantics.md` | Identity, coordinate, transaction, and read rules | Approved |
| 3 | `01-api-contract.md` | Authoritative HTTP and JSON contract | Approved |
| 4 | `02-schema.sql` | Executable target PostgreSQL DDL | Approved |
| 5 | `04-operations.md` | Runtime, deployment, security, and observability | Approved |
| 6 | `05-test-and-acceptance.md` | Required verification and release gates | Approved |
| 7 | `openapi.json` | Machine-readable projection of the stable HTTP contract | Added after core implementation |

The more specialized document governs when statements overlap. Resolve a conflict by updating every affected document before changing code; do not silently choose one interpretation in the implementation.

Design rationale stays beside the relevant rule. No legacy compatibility, data migration, or separate ADR set is required.
