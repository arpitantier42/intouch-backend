-- pgTAP tests for blocks, connections, follows and mentorship requests.
-- Run with: supabase test db
begin;
create extension if not exists pgtap with schema extensions;
select plan(12);

insert into auth.users (id) values
  ('00000000-0000-0000-0000-0000000000a1'),
  ('00000000-0000-0000-0000-0000000000b1'),
  ('00000000-0000-0000-0000-0000000000c1');
insert into public.profiles (id, full_name, profile_visibility) values
  ('00000000-0000-0000-0000-0000000000a1', 'Alice A', 'public'),
  ('00000000-0000-0000-0000-0000000000b1', 'Bob B', 'public'),
  ('00000000-0000-0000-0000-0000000000c1', 'Carol C', 'connections');

set local role authenticated;

-- Alice requests Bob.
set local "request.jwt.claims" = '{"sub":"00000000-0000-0000-0000-0000000000a1","role":"authenticated"}';
select lives_ok($$ select public.send_connection_request('00000000-0000-0000-0000-0000000000b1') $$,
  'Alice sends a connection request');
select is(public.connection_state('00000000-0000-0000-0000-0000000000b1'), 'sent', 'Alice sees Sent');
select lives_ok($$ select public.follow_user('00000000-0000-0000-0000-0000000000b1') $$, 'Alice follows Bob');
select throws_ok($$ select public.follow_user('00000000-0000-0000-0000-0000000000c1') $$, 'PT403', null,
  'a connections-only profile cannot be followed');
select throws_ok($$ select public.request_mentorship('00000000-0000-0000-0000-0000000000b1', null, 'hi') $$,
  'PT400', null, 'a mentorship request needs a topic');

-- Bob sees the request as Incoming, then blocks Alice.
set local "request.jwt.claims" = '{"sub":"00000000-0000-0000-0000-0000000000b1","role":"authenticated"}';
select is(public.connection_state('00000000-0000-0000-0000-0000000000a1'), 'incoming', 'Bob sees Incoming');
select is((select followers from public.follow_counts(auth.uid())), 1, 'Bob has one follower');
select lives_ok($$ select public.block_user('00000000-0000-0000-0000-0000000000a1') $$, 'Bob blocks Alice');
select is((select count(*)::int from public.connections), 0, 'blocking deletes the connection');
select is((select count(*)::int from public.blocks), 1, 'Alice is in Bob''s blocked accounts');

-- Alice is blocked; after Bob unblocks, the pair is Idle.
set local "request.jwt.claims" = '{"sub":"00000000-0000-0000-0000-0000000000a1","role":"authenticated"}';
select throws_ok($$ select public.send_connection_request('00000000-0000-0000-0000-0000000000b1') $$,
  'PT403', null, 'a blocked user cannot connect');
set local "request.jwt.claims" = '{"sub":"00000000-0000-0000-0000-0000000000b1","role":"authenticated"}';
select public.unblock_user('00000000-0000-0000-0000-0000000000a1');
select is(public.connection_state('00000000-0000-0000-0000-0000000000a1'), 'idle', 'unblock leaves Idle');

select * from finish();
rollback;
