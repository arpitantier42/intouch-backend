-- pgTAP policy tests for profiles, communities, memberships, questions,
-- answers and invitations. Run with: supabase test db
begin;
create extension if not exists pgtap with schema extensions;
select plan(22);

-- Fixtures (as postgres, bypassing RLS).
insert into auth.users (id) values
  ('00000000-0000-0000-0000-00000000000a'),
  ('00000000-0000-0000-0000-00000000000b'),
  ('00000000-0000-0000-0000-00000000000c');
insert into public.profiles (id, full_name) values
  ('00000000-0000-0000-0000-00000000000a', 'Alice A'),
  ('00000000-0000-0000-0000-00000000000b', 'Bob B'),
  ('00000000-0000-0000-0000-00000000000c', 'Carol C');

select ok(
  not exists (select 1 from pg_class c join pg_namespace n on n.oid = c.relnamespace
              where n.nspname = 'public' and c.relkind = 'r' and not c.relrowsecurity),
  'RLS is enabled on every public table');

-- Alice creates A (private); Bob creates B (public); Alice joins B as member.
set local role authenticated;
set local "request.jwt.claims" = '{"sub":"00000000-0000-0000-0000-00000000000a","role":"authenticated"}';
select lives_ok(
  $$ select public.create_community('Alpha', 'online', '10000000-0000-0000-0000-000000000001') $$,
  'alice creates a private community');
set local "request.jwt.claims" = '{"sub":"00000000-0000-0000-0000-00000000000b","role":"authenticated"}';
select lives_ok(
  $$ select public.create_community('Beta', 'online', '10000000-0000-0000-0000-000000000002', null, 'public') $$,
  'bob creates a public community');
reset role;

insert into public.memberships (community_id, user_id, role, status, via)
select id, '00000000-0000-0000-0000-00000000000a', 'member', 'joined', 'direct'
from public.communities where name = 'Beta';

select results_eq(
  $$ select role || '/' || status || '/' || via from public.memberships m
     join public.communities c on c.id = m.community_id
     where c.name = 'Alpha' and m.user_id = '00000000-0000-0000-0000-00000000000a' $$,
  array['owner/joined/direct'],
  'creator is registered as owner, joined, via direct');

select is((select member_count from public.communities where name = 'Beta'), 2, 'member_count counts joined members');

select throws_ok(
  $$ update public.memberships set role = 'superadmin' where user_id = '00000000-0000-0000-0000-00000000000a' $$,
  '23514', null, 'role outside owner|admin|moderator|member is rejected');
select throws_ok(
  $$ update public.memberships set status = 'banned_x' where user_id = '00000000-0000-0000-0000-00000000000a' $$,
  '23514', null, 'status outside pending|joined is rejected');
select throws_ok(
  $$ update public.memberships m set role = 'owner' from public.communities c
     where c.id = m.community_id and c.name = 'Beta' and m.user_id = '00000000-0000-0000-0000-00000000000a' $$,
  '23505', null, 'a second owner is rejected by the partial unique index');
select throws_ok(
  $$ insert into public.community_questions (community_id, label, kind)
     select id, 'CV', 'file' from public.communities where name = 'Beta' $$,
  '23514', null, 'question kind file is rejected');
select throws_ok(
  $$ insert into public.community_questions (community_id, label, kind)
     select id, 'Pick', 'single_choice' from public.communities where name = 'Beta' $$,
  '23514', null, 'choice question without options is rejected');

-- As Alice (member of both).
set local role authenticated;
set local "request.jwt.claims" = '{"sub":"00000000-0000-0000-0000-00000000000a","role":"authenticated"}';
select is((select count(*)::int from public.communities), 2, 'a member of two communities sees both');
select throws_ok(
  $$ select public.transfer_ownership((select id from public.communities where name = 'Beta'),
                                      '00000000-0000-0000-0000-00000000000a') $$,
  'PT403', null, 'owner of A cannot act as owner in B where they are a member');
select throws_ok(
  $$ update public.memberships set role = 'owner' where user_id = auth.uid() $$,
  '42501', null, 'API users cannot write role');
select throws_ok(
  $$ update public.profiles set username = 'alice a' where id = auth.uid() $$,
  '23514', null, 'username with a space is rejected');
select throws_ok(
  $$ update public.profiles set username = 'al' where id = auth.uid() $$,
  '23514', null, 'two-character username is rejected');
select throws_ok(
  $$ update public.profiles set bio = repeat('x', 200) where id = auth.uid() $$,
  '22001', null, '200-character bio is rejected');
select lives_ok(
  $$ update public.profiles set username = 'alice' where id = auth.uid() $$,
  'valid username saves');
select throws_ok(
  $$ update public.profiles set is_operator = true where id = auth.uid() $$,
  '42501', null, 'is_operator is server-set');

-- As Carol (no memberships).
set local "request.jwt.claims" = '{"sub":"00000000-0000-0000-0000-00000000000c","role":"authenticated"}';
select is((select count(*)::int from public.communities where name = 'Alpha'), 0, 'non-member sees no private community');
select is((select count(*)::int from public.memberships where user_id <> auth.uid()), 0, 'non-member sees no memberships');
select throws_ok(
  $$ insert into public.memberships (community_id, user_id, role, status, via)
     select id, auth.uid(), 'member', 'joined', 'direct' from public.communities where name = 'Beta' $$,
  '42501', null, 'self-inserting a joined membership is rejected');
select throws_ok(
  $$ update public.profiles set username = 'alice' where id = auth.uid() $$,
  '23505', null, 'a taken username is rejected by the unique index');

reset role;
select * from finish();
rollback;
