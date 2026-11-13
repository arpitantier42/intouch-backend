-- pgTAP tests for the RSVP RPCs (set_rsvp, verify_rsvp_payment).
-- Run with: supabase test db
begin;
create extension if not exists pgtap with schema extensions;
select plan(7);

insert into auth.users (id) values
  ('00000000-0000-0000-0000-0000000000a6'),
  ('00000000-0000-0000-0000-0000000000b6');
insert into public.profiles (id, full_name) values
  ('00000000-0000-0000-0000-0000000000a6', 'Alice A'),
  ('00000000-0000-0000-0000-0000000000b6', 'Bob B');

set local role anon;
select throws_ok($$ select public.set_rsvp(gen_random_uuid(), 'going') $$, '42501', null,
  'visitors cannot RSVP');
select throws_ok($$ select public.verify_rsvp_payment(gen_random_uuid(), gen_random_uuid()) $$, '42501', null,
  'visitors cannot verify payments');

set local role authenticated;
set local "request.jwt.claims" = '{"sub":"00000000-0000-0000-0000-0000000000a6","role":"authenticated"}';
select throws_ok($$ select public._rsvp_json(null::public.rsvps) $$, '42501', null,
  'clients cannot call the internal RSVP serialiser');
select throws_ok($$ select public.set_rsvp(gen_random_uuid(), 'maybe') $$, 'PT400', null,
  'unknown status is rejected');
select throws_ok($$ select public.set_rsvp(gen_random_uuid(), 'going') $$, 'PT404', null,
  'unknown event is not found');
select throws_ok($$ select public.set_rsvp(gen_random_uuid(), 'declined', null, 'bored') $$, 'PT400', null,
  'unknown decline reason is rejected');
select is(public._rsvp_badge('going', 'pay_later', false), 'Payment pending', 'pay later reads Payment pending');

select * from finish();
