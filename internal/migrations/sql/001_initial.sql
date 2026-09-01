create schema hub;

create function hub.normalize_code(p_value text)
returns text
language sql
immutable
strict
parallel safe
as $function$
    select normalize(lower(normalize(btrim(p_value), NFKC)), NFKC);
$function$;

create table hub.providers (
    provider_code text primary key,
    provider_key text generated always as (hub.normalize_code(provider_code)) stored,
    check (length(btrim(provider_code)) > 0),
    unique (provider_key)
);

create table hub.datasets (
    dataset_id bigint generated always as identity primary key,
    provider_code text not null references hub.providers(provider_code) on delete cascade,
    dataset_code text not null,
    dataset_key text generated always as (hub.normalize_code(dataset_code)) stored,
    source_stamp jsonb,
    observation_count bigint not null default 0 check (observation_count >= 0),
    observations_updated_at timestamptz,
    load_status text not null default 'loading' check (load_status in ('loading', 'ready')),
    check (length(btrim(dataset_code)) > 0),
    unique (provider_code, dataset_key)
);

create table hub.dimensions (
    dimension_id bigint generated always as identity primary key,
    dataset_id bigint not null references hub.datasets(dataset_id) on delete cascade,
    code text not null,
    dimension_key text generated always as (hub.normalize_code(code)) stored,
    position integer not null check (position >= 0),
    size integer not null check (size > 0),
    check (length(btrim(code)) > 0),
    unique (dataset_id, dimension_key),
    unique (dataset_id, position)
);

create table hub.categories (
    category_id bigint generated always as identity primary key,
    dimension_id bigint not null references hub.dimensions(dimension_id) on delete cascade,
    code text not null,
    category_key text generated always as (hub.normalize_code(code)) stored,
    position integer not null check (position >= 0),
    check (length(btrim(code)) > 0),
    unique (dimension_id, category_key),
    unique (dimension_id, position)
);

create table hub.dataset_structure_cache (
    dataset_id bigint primary key references hub.datasets(dataset_id) on delete cascade,
    structure jsonb not null check (jsonb_typeof(structure) = 'array')
);

create table hub.observations (
    dataset_id bigint not null references hub.datasets(dataset_id) on delete cascade,
    cell_index bigint not null check (cell_index >= 0),
    value numeric,
    status_code text,
    primary key (dataset_id, cell_index),
    check (value is not null or status_code is not null)
) partition by hash (dataset_id);

do $partitions$
declare
    partition_number integer;
begin
    for partition_number in 0..31 loop
        execute format(
            'create table hub.observations_p%s partition of hub.observations for values with (modulus 32, remainder %s)',
            partition_number,
            partition_number
        );
    end loop;
end;
$partitions$;

create function hub.guard_provider_identity()
returns trigger
language plpgsql
as $function$
begin
    if old.provider_code is distinct from new.provider_code then
        raise exception 'A provider identity cannot be changed';
    end if;
    return new;
end;
$function$;

create function hub.guard_dataset_identity()
returns trigger
language plpgsql
as $function$
begin
    if old.provider_code is distinct from new.provider_code
       or old.dataset_code is distinct from new.dataset_code then
        raise exception 'A dataset identity cannot be changed';
    end if;
    return new;
end;
$function$;

create function hub.guard_dimension_write()
returns trigger
language plpgsql
as $function$
declare
    status text;
begin
    select load_status into status from hub.datasets where dataset_id = new.dataset_id;
    if status <> 'loading' then
        raise exception 'Dimensions can only be inserted or updated while a dataset is loading';
    end if;
    if tg_op = 'UPDATE' and old.dataset_id is distinct from new.dataset_id then
        raise exception 'A dimension cannot be moved to another dataset';
    end if;
    return new;
end;
$function$;

create function hub.guard_category_write()
returns trigger
language plpgsql
as $function$
declare
    status text;
    dimension_size integer;
begin
    select d.load_status, dm.size into status, dimension_size
    from hub.dimensions dm join hub.datasets d using (dataset_id)
    where dm.dimension_id = new.dimension_id;
    if status <> 'loading' then
        raise exception 'Categories can only be inserted or updated while a dataset is loading';
    end if;
    if new.position >= dimension_size then
        raise exception 'Category position is outside dimension size';
    end if;
    if tg_op = 'UPDATE' and old.dimension_id is distinct from new.dimension_id then
        raise exception 'A category cannot be moved to another dimension';
    end if;
    return new;
end;
$function$;

create trigger providers_guard_identity
before update on hub.providers
for each row execute function hub.guard_provider_identity();

create trigger datasets_guard_identity
before update on hub.datasets
for each row execute function hub.guard_dataset_identity();

create trigger dimensions_guard_write
before insert or update on hub.dimensions
for each row execute function hub.guard_dimension_write();

create trigger categories_guard_write
before insert or update on hub.categories
for each row execute function hub.guard_category_write();

create view hub.provider_summary as
select p.provider_code,
       count(d.dataset_id) filter (where d.load_status = 'ready')::bigint as dataset_count
from hub.providers p
left join hub.datasets d using (provider_code)
group by p.provider_code, p.provider_key;

create view hub.dataset_summary as
select dataset_id, provider_code, dataset_code, observation_count, observations_updated_at
from hub.datasets
where load_status = 'ready';

create view hub.dataset_metadata as
select d.dataset_id, d.provider_code, d.dataset_code, d.source_stamp,
       d.observation_count, d.observations_updated_at, c.structure
from hub.datasets d
join hub.dataset_structure_cache c using (dataset_id)
where d.load_status = 'ready';

---- create above / drop below ----

drop schema hub cascade;

