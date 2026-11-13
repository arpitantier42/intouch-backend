-- pgTAP tests for the chat threads RPCs.
-- Run with: supabase test db
begin;
create extension if not exists pgtap with schema extensions;
select plan(9);

insert into auth.users (id) values
  ('00000000-0000-0000-0000-0000000000c1'), ('00000000-0000-0000-0000-0000000000c2'),
  ('00000000-0000-0000-0000-0000000000c3');
insert into public.profiles (id, full_name) values
  ('00000000-0000-0000-0000-0000000000c1', 'Ana'), ('00000000-0000-0000-0000-0000000000c2', 'Ben'),
  ('00000000-0000-0000-0000-0000000000c3', 'Cy Outsider');
insert into public.communities (id, handle, name, location_type, visibility, created_by, creation_key) values
  ('21000000-0000-0000-0000-000000000001', 'chatco', 'ChatCo', 'online', 'private',
   '00000000-0000-0000-0000-0000000000c1', '31000000-0000-0000-0000-000000000001');
insert into public.memberships (community_id, user_id, role, status, via) values
  ('21000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-0000000000c1', 'owner', 'joined', 'direct'),
  ('21000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-0000000000c2', 'member', 'joined', 'direct');

set local role authenticated;
set local "request.jwt.claims" = '{"sub":"00000000-0000-0000-0000-0000000000c1","role":"authenticated"}';

select lives_ok($$select public.open_dm('21000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-0000000000c2')$$,
  'a member opens a DM with another member');
select is((select count(*)::int from public.threads), 1, 'the DM is visible to its participant');
select lives_ok($$select public.open_dm('21000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-0000000000c2')$$,
  'reopening reuses the DM');
select is((select count(*)::int from public.threads), 1, 'no duplicate DM');
select throws_ok($$select public.open_dm('21000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-0000000000c3')$$,
  'PT403', null, 'a non-member cannot be messaged');
select throws_ok($$select public._check_group_members('21000000-0000-0000-0000-000000000001', array['00000000-0000-0000-0000-0000000000c2'::uuid])$$,
  '42501', null, 'internal helpers are not callable by clients');

set local "request.jwt.claims" = '{"sub":"00000000-0000-0000-0000-0000000000c3","role":"authenticated"}';
select throws_ok($$select public.list_threads('21000000-0000-0000-0000-000000000001', 'all', null)$$,
  'PT403', null, 'a non-member cannot list the community''s chats');
select is((select count(*)::int from public.threads), 0, 'a non-member sees no threads');

set local role anon;
select throws_ok($$select public.list_threads('21000000-0000-0000-0000-000000000001', 'all', null)$$,
  '42501', null, 'visitors cannot call chat RPCs');

select * from finish();
rollback;
