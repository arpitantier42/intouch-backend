-- pgTAP tests for chat threads, participants, messages, marks and call logs.
-- Run with: supabase test db
begin;
create extension if not exists pgtap with schema extensions;
select plan(10);

insert into auth.users (id) values
  ('00000000-0000-0000-0000-0000000000a2'),
  ('00000000-0000-0000-0000-0000000000b2'),
  ('00000000-0000-0000-0000-0000000000c2');
insert into public.profiles (id, full_name) values
  ('00000000-0000-0000-0000-0000000000a2', 'Alice A'),
  ('00000000-0000-0000-0000-0000000000b2', 'Bob B'),
  ('00000000-0000-0000-0000-0000000000c2', 'Carol C');

-- Alice owns community A; Bob and Carol are joined.
set local role authenticated;
set local "request.jwt.claims" = '{"sub":"00000000-0000-0000-0000-0000000000a2","role":"authenticated"}';
select public.create_community('Chat A', 'online', '20000000-0000-0000-0000-000000000001');
reset role;
insert into public.memberships (community_id, user_id, role, status, via)
select c.id, u.id, 'member', 'joined', 'direct'
from public.communities c,
     (values ('00000000-0000-0000-0000-0000000000b2'::uuid), ('00000000-0000-0000-0000-0000000000c2'::uuid)) u (id)
where c.name = 'Chat A';

-- A DM between Alice and Bob, seeded as the open-DM RPC would.
insert into public.threads (id, community_id, kind, dm_key, created_by)
select '30000000-0000-0000-0000-000000000001', c.id, 'dm',
       public._dm_key(c.id, '00000000-0000-0000-0000-0000000000a2', '00000000-0000-0000-0000-0000000000b2'),
       '00000000-0000-0000-0000-0000000000a2'
from public.communities c where c.name = 'Chat A';
insert into public.thread_participants (thread_id, user_id) values
  ('30000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-0000000000a2'),
  ('30000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-0000000000b2');
insert into public.messages (id, thread_id, sender_id, body) values
  ('40000000-0000-0000-0000-000000000001', '30000000-0000-0000-0000-000000000001',
   '00000000-0000-0000-0000-0000000000a2', 'hello');
insert into public.message_marks (message_id, user_id, kind) values
  ('40000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-0000000000b2', 'star');

select throws_ok(
  $$ insert into public.messages (thread_id, sender_id, body)
     values ('30000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-0000000000a2', '   ') $$,
  '23514', null, 'whitespace-only text is rejected');

select ok(
  (select last_message_at is not null and last_message_preview = 'hello'
   from public.threads where id = '30000000-0000-0000-0000-000000000001'),
  'a message moves the thread to the top');

set local role authenticated;
set local "request.jwt.claims" = '{"sub":"00000000-0000-0000-0000-0000000000b2","role":"authenticated"}';
select is((select unread_count from public.thread_participants
           where user_id = auth.uid()), 1, 'recipient has one unread');
select is(public.message_status('40000000-0000-0000-0000-000000000001'), 'sent',
          'unopened message is not read');
select lives_ok(
  $$ update public.thread_participants set last_read_at = now() where user_id = auth.uid() $$,
  'recipient opens the thread');
select is((select unread_count from public.thread_participants
           where user_id = auth.uid()), 0, 'opening resets unread');
select throws_ok(
  $$ insert into public.messages (thread_id, sender_id, body)
     values ('30000000-0000-0000-0000-000000000001', auth.uid(), 'hi') $$,
  '42501', null, 'clients cannot insert messages directly');

set local "request.jwt.claims" = '{"sub":"00000000-0000-0000-0000-0000000000a2","role":"authenticated"}';
select is((select count(*)::int from public.message_marks), 0, 'another user''s star is invisible');

set local "request.jwt.claims" = '{"sub":"00000000-0000-0000-0000-0000000000c2","role":"authenticated"}';
select is((select count(*)::int from public.threads), 0, 'non-participant sees no threads');
select is((select count(*)::int from public.messages), 0, 'non-participant sees no messages');

reset role;
select * from finish();
rollback;
