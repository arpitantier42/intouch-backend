-- Minimal stand-in for the parts of a Supabase database that the migrations
-- rely on, so they can be applied to a plain Postgres (local tests, CI without
-- the Supabase CLI). Never apply this to a real Supabase project.

-- Roles are cluster-wide and test databases are built in parallel (go test
-- runs packages concurrently), so a concurrent create is tolerated.
do $$
declare
  r text;
begin
  foreach r in array array['anon', 'authenticated', 'service_role'] loop
    if not exists (select 1 from pg_roles where rolname = r) then
      begin
        execute format('create role %I nologin noinherit%s', r,
          case when r = 'service_role' then ' bypassrls' else '' end);
      exception when duplicate_object or unique_violation then
        null;
      end;
    end if;
  end loop;
end
$$;

create schema if not exists auth;
create schema if not exists extensions;

create table if not exists auth.users (
  id    uuid primary key default gen_random_uuid(),
  email text unique
);
-- Supabase stores the confirmed phone without '+' (e.g. 919876543210).
alter table auth.users add column if not exists phone text;
alter table auth.users add column if not exists phone_confirmed_at timestamptz;

-- Same contract as Supabase: the subject of the request's JWT claims.
create or replace function auth.uid() returns uuid
language sql stable as $$
  select coalesce(
    nullif(current_setting('request.jwt.claim.sub', true), ''),
    (nullif(current_setting('request.jwt.claims', true), '')::jsonb ->> 'sub')
  )::uuid
$$;

grant usage on schema auth, public, extensions to anon, authenticated, service_role;
grant execute on function auth.uid() to anon, authenticated, service_role;

-- Supabase grants everything on new public objects to the API roles; RLS and
-- the migrations' explicit revokes are what restrict them.
alter default privileges in schema public grant all on tables to anon, authenticated, service_role;
alter default privileges in schema public grant all on sequences to anon, authenticated, service_role;
alter default privileges in schema public grant execute on functions to anon, authenticated, service_role;
