-- A tiny subset of pgTAP for running supabase/tests/database/*.test.sql on a
-- Postgres without the pgtap extension (see internal/dbtest). A failing
-- assertion raises immediately with its description. Supported:
-- plan, ok, is, results_eq (single column), throws_ok, lives_ok, finish.

create or replace function public.plan(integer) returns text
language sql as $$ select '1..' || $1 $$;

create or replace function public.ok(boolean, text) returns text
language plpgsql as $$
begin
  if $1 is not true then
    raise exception 'not ok: %', $2 using errcode = 'PTFAI';
  end if;
  return 'ok - ' || $2;
end $$;

create or replace function public.is(anyelement, anyelement, text) returns text
language plpgsql as $$
begin
  if $1 is distinct from $2 then
    raise exception 'not ok: % (have %, want %)', $3, $1, $2 using errcode = 'PTFAI';
  end if;
  return 'ok - ' || $3;
end $$;

create or replace function public.results_eq(text, anyarray, text) returns text
language plpgsql as $$
declare
  v_have text;
begin
  execute 'select array_agg(x)::text from (' || $1 || ') t(x)' into v_have;
  if v_have is distinct from $2::text then
    raise exception 'not ok: % (have %, want %)', $3, v_have, $2 using errcode = 'PTFAI';
  end if;
  return 'ok - ' || $3;
end $$;

create or replace function public.throws_ok(text, text, text, text) returns text
language plpgsql as $$
begin
  begin
    execute $1;
  exception when others then
    if sqlstate = 'PTFAI' then raise; end if;
    if sqlstate <> $2 then
      raise exception 'not ok: % (threw % %, want %)', $4, sqlstate, sqlerrm, $2 using errcode = 'PTFAI';
    end if;
    return 'ok - ' || $4;
  end;
  raise exception 'not ok: % (no error, want %)', $4, $2 using errcode = 'PTFAI';
end $$;

create or replace function public.lives_ok(text, text) returns text
language plpgsql as $$
begin
  execute $1;
  return 'ok - ' || $2;
exception when others then
  raise exception 'not ok: % (threw % %)', $2, sqlstate, sqlerrm using errcode = 'PTFAI';
end $$;

create or replace function public.finish() returns setof text
language sql as $$ select null::text where false $$;

grant execute on all functions in schema public to public;
