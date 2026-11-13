-- pgTAP tests for the messaging RPCs (send, list, edit, delete, marks).
-- Run with: supabase test db
begin;
create extension if not exists pgtap with schema extensions;
select plan(10);

insert into auth.users (id) values
  ('00000000-0000-0000-0000-0000000000d1'), ('00000000-0000-0000-0000-0000000000d2');
insert into public.profiles (id, full_name) values
  ('00000000-0000-0000-0000-0000000000d1', 'Dee'), ('00000000-0000-0000-0000-0000000000d2', 'Eli');
insert into public.communities (id, handle, name, location_type, visibility, created_by, creation_key) values
  ('22000000-0000-0000-0000-000000000001', 'msgco', 'MsgCo', 'online', 'private',
   '00000000-0000-0000-0000-0000000000d1', '32000000-0000-0000-0000-000000000001');
insert into public.memberships (community_id, user_id, role, status, via) values
  ('22000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-0000000000d1', 'owner', 'joined', 'direct'),
  ('22000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-0000000000d2', 'member', 'joined', 'direct');

set local role anon;
select throws_ok($$select public.send_message(gen_random_uuid(), gen_random_uuid(), 'text', 'hi')$$,
  '42501', null, 'anon cannot call send_message');
reset role;

set local role authenticated;
set local "request.jwt.claims" = '{"sub":"00000000-0000-0000-0000-0000000000d1","role":"authenticated"}';
create temp table t_ids as
  select (public.open_dm('22000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-0000000000d2')).id as tid;

select throws_ok($$select public.send_message((select tid from t_ids), '22000000-0000-0000-0000-000000000001', 'text', '   ')$$,
  'PT400', null, 'whitespace-only text is rejected');
select throws_ok($$select public.send_message((select tid from t_ids), gen_random_uuid(), 'text', 'hi')$$,
  'PT403', null, 'a send scoped to another community is rejected');
select throws_ok($$select public.send_message((select tid from t_ids), '22000000-0000-0000-0000-000000000001', 'document', null, 'a.pdf', 26214401)$$,
  'PT413', null, 'an attachment over 25 MB is rejected');
select lives_ok($$select public.send_message((select tid from t_ids), '22000000-0000-0000-0000-000000000001', 'text', 'hello')$$,
  'a member sends a text');
select is((select count(*)::int from public.messages), 1, 'the message is stored');
select is((select public.message_status(id) from public.messages limit 1), 'sent', 'unopened by the peer: not read');

set local "request.jwt.claims" = '{"sub":"00000000-0000-0000-0000-0000000000d2","role":"authenticated"}';
select throws_ok($$select public.edit_message((select id from public.messages limit 1), 'mine now')$$,
  'PT403', null, 'a non-author cannot edit');
select is(jsonb_array_length(public.list_messages((select tid from t_ids))), 1, 'the peer pages the thread');
select is((select unread_count from public.thread_participants
           where user_id = '00000000-0000-0000-0000-0000000000d2'), 0, 'opening resets unread');

select * from finish();
rollback;
