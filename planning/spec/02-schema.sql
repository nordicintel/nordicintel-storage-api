-- Target Database Schema
-- PostgreSQL 15+ target DDL for the redesigned storage service.
-- This file is an executable schema specification, not a migration.

begin;

create schema storage;

comment on schema storage is
    'Durable current-state storage for NordicIntel observation datasets.';

create table storage.providers (
    provider_id bigint generated always as identity,
    provider_code text not null,
    provider_key text collate "C" not null,

    constraint providers_pkey primary key (provider_id),
    constraint providers_code_not_empty check (provider_code <> ''),
    constraint providers_key_not_empty check (provider_key <> ''),
    constraint providers_key_unique unique (provider_key)
);

comment on table storage.providers is
    'Internal provider identity registry; rows may remain when no current datasets reference them.';
comment on column storage.providers.provider_code is
    'Provider spelling preserved from its first successful creation.';
comment on column storage.providers.provider_key is
    'Application-generated Unicode-normalized identity key used for matching and bytewise ordering.';

create table storage.datasets (
    dataset_id bigint generated always as identity,
    provider_id bigint not null,
    dataset_code text not null,
    dataset_key text collate "C" not null,
    source_stamp jsonb not null,
    cell_count bigint not null,
    valued_cell_count bigint not null,
    null_cell_count bigint generated always as (cell_count - valued_cell_count) stored,
    updated_at timestamp with time zone not null,

    constraint datasets_pkey primary key (dataset_id),
    constraint datasets_provider_fkey
        foreign key (provider_id)
        references storage.providers (provider_id)
        on delete restrict,
    constraint datasets_code_not_empty check (dataset_code <> ''),
    constraint datasets_key_not_empty check (dataset_key <> ''),
    constraint datasets_identity_unique unique (provider_id, dataset_key),
    constraint datasets_cell_count_range check (cell_count between 1 and 1000000),
    constraint datasets_valued_cell_count_range
        check (valued_cell_count between 0 and cell_count)
);

comment on table storage.datasets is
    'Current accepted state and cached whole-dataset counts for one provider/dataset identity.';
comment on column storage.datasets.dataset_code is
    'Dataset spelling preserved from its first successful creation.';
comment on column storage.datasets.dataset_key is
    'Application-generated Unicode-normalized key unique within a provider.';
comment on column storage.datasets.source_stamp is
    'Required opaque JSON value; the JSON literal null is distinct from forbidden SQL NULL.';
comment on column storage.datasets.cell_count is
    'Logical Cartesian product of all current category counts.';
comment on column storage.datasets.valued_cell_count is
    'Logical cells containing either a numeric value or a text value.';
comment on column storage.datasets.null_cell_count is
    'Generated count of logical cells containing neither a numeric nor text value.';
comment on column storage.datasets.updated_at is
    'Database time at which the current replacement completed successfully.';

create table storage.dimensions (
    dimension_id bigint generated always as identity,
    dataset_id bigint not null,
    dimension_code text not null,
    dimension_key text collate "C" not null,
    position integer not null,

    constraint dimensions_pkey primary key (dimension_id),
    constraint dimensions_dataset_fkey
        foreign key (dataset_id)
        references storage.datasets (dataset_id)
        on delete cascade,
    constraint dimensions_code_not_empty check (dimension_code <> ''),
    constraint dimensions_key_not_empty check (dimension_key <> ''),
    constraint dimensions_position_nonnegative check (position >= 0),
    constraint dimensions_key_unique unique (dataset_id, dimension_key),
    constraint dimensions_position_unique unique (dataset_id, position)
);

comment on table storage.dimensions is
    'Current dataset dimensions in deterministic normalized-key order.';
comment on column storage.dimensions.dimension_code is
    'Dimension spelling supplied by the latest successful replacement.';
comment on column storage.dimensions.dimension_key is
    'Application-generated Unicode-normalized key unique within a dataset.';
comment on column storage.dimensions.position is
    'Zero-based internal row-major position; not a business presentation order.';

create table storage.categories (
    category_id bigint generated always as identity,
    dimension_id bigint not null,
    category_code text not null,
    category_key text collate "C" not null,
    position integer not null,

    constraint categories_pkey primary key (category_id),
    constraint categories_dimension_fkey
        foreign key (dimension_id)
        references storage.dimensions (dimension_id)
        on delete cascade,
    constraint categories_code_not_empty check (category_code <> ''),
    constraint categories_key_not_empty check (category_key <> ''),
    constraint categories_position_nonnegative check (position >= 0),
    constraint categories_key_unique unique (dimension_id, category_key),
    constraint categories_position_unique unique (dimension_id, position)
);

comment on table storage.categories is
    'Current dimension categories in deterministic normalized-key order.';
comment on column storage.categories.category_code is
    'Category spelling supplied by the latest successful replacement.';
comment on column storage.categories.category_key is
    'Application-generated Unicode-normalized key unique within a dimension.';
comment on column storage.categories.position is
    'Zero-based internal position used to calculate row-major cell indexes.';

create table storage.observations (
    dataset_id bigint not null,
    cell_index bigint not null,
    numeric_value double precision,
    text_value text,
    status_code text,

    constraint observations_pkey primary key (dataset_id, cell_index),
    constraint observations_dataset_fkey
        foreign key (dataset_id)
        references storage.datasets (dataset_id)
        on delete cascade,
    constraint observations_cell_index_range
        check (cell_index between 0 and 999999),
    constraint observations_has_content
        check (
            numeric_value is not null
            or text_value is not null
            or status_code is not null
        ),
    constraint observations_value_exclusive
        check (numeric_value is null or text_value is null),
    constraint observations_numeric_value_finite
        check (
            numeric_value is null
            or (
                numeric_value > '-Infinity'::double precision
                and numeric_value < 'Infinity'::double precision
            )
        )
) partition by hash (dataset_id);

comment on table storage.observations is
    'Sparse current observations partitioned so every dataset is routed to one hash partition.';
comment on column storage.observations.cell_index is
    'Internal row-major index derived from normalized dimension and category positions.';
comment on column storage.observations.numeric_value is
    'Finite IEEE 754 binary64 value; mutually exclusive with text_value.';
comment on column storage.observations.text_value is
    'Optional non-numeric cell value; mutually exclusive with numeric_value.';
comment on column storage.observations.status_code is
    'Optional observation-level status independent of the value channels.';

do $partitions$
declare
    partition_number integer;
begin
    for partition_number in 0..31 loop
        execute format(
            'create table storage.observations_p%s partition of storage.observations for values with (modulus 32, remainder %s)',
            partition_number,
            partition_number
        );
    end loop;
end;
$partitions$;

commit;
