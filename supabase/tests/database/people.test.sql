-- pgTAP tests for people search and the member profile.
-- Run with: supabase test db
begin;
create extension if not exists pgtap with schema extensions;
select plan(7);

insert into auth.users (id) values
  ('00000000-0000-0000-0000-0000000000a5'),
  ('00000000-0000-0000-0000-0000000000b5');
insert into public.profiles (id, full_name) values
  ('00000000-0000-0000-0000-0000000000a5', 'Alice A'),
  ('00000000-0000-0000-0000-0000000000b5', 'Bob B');

set local role anon;
select throws_ok($$ select public.search_people('members') $$, '42501', null,
  'visitors cannot search people');
select throws_ok($$ select public.member_profile('00000000-0000-0000-0000-0000000000b5') $$, '42501', null,
  'visitors cannot read member profiles');

set local role authenticated;
set local "request.jwt.claims" = '{"sub":"00000000-0000-0000-0000-0000000000a5","role":"authenticated"}';
select throws_ok($$ select public._connected_ids('00000000-0000-0000-0000-0000000000b5') $$, '42501', null,
  'clients cannot list another user''s connections directly');
select throws_ok($$ select public.search_people('everyone') $$, 'PT400', null, 'unknown mode is rejected');
select throws_ok($$ select public.search_people('members') $$, 'PT400', null, 'members mode needs a community');
select is(public.search_people('suggested'), '[]'::jsonb, 'no shared community → no suggestions');
select throws_ok($$ select public.member_profile('00000000-0000-0000-0000-0000000000b5') $$, 'PT404', null,
  'no shared community → profile not found');

select * from finish();
rollback;
