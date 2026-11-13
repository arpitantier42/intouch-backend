-- pgTAP tests for auth_attempts, known_devices and notifications: throttles,
-- lockout and what clients may read. Run with: supabase test db
begin;
create extension if not exists pgtap with schema extensions;
select plan(12);

insert into auth.users (id) values
  ('00000000-0000-0000-0000-0000000000d1'),
  ('00000000-0000-0000-0000-0000000000d2');
insert into public.profiles (id, full_name) values
  ('00000000-0000-0000-0000-0000000000d1', 'Dana D'),
  ('00000000-0000-0000-0000-0000000000d2', 'Eli E');

-- The service (service_role) drives the throttles.
set local role service_role;
select lives_ok($$ select public.auth_otp_send(repeat('a', 64)) $$, 'first OTP send is accepted');
select throws_ok($$ select public.auth_otp_send(repeat('a', 64)) $$, 'PT429', null,
  'a resend within 30 s is throttled');
select lives_ok($$ select public.auth_otp_verify_begin(repeat('a', 64)) from generate_series(1, 5) $$,
  'five verifies on one OTP are allowed');
select throws_ok($$ select public.auth_otp_verify_begin(repeat('a', 64)) $$, 'PT429', null,
  'the 6th verify on one OTP is rejected');
select lives_ok($$ select public.auth_password_begin(repeat('b', 64)) from generate_series(1, 10) $$,
  'ten failed password attempts are logged');
select throws_ok($$ select public.auth_password_begin(repeat('b', 64)) $$, 'PT423', null,
  'the 11th attempt is blocked');
select is((select (public.record_sign_in('00000000-0000-0000-0000-0000000000d1',
  '20000000-0000-0000-0000-000000000001', repeat('c', 64), 'Phone') ->> 'alerted')::boolean),
  true, 'a sign-in from a new device stores a login alert');

-- Clients: no auth log, own devices without the hash, own notifications.
reset role;
set local role authenticated;
set local "request.jwt.claims" = '{"sub":"00000000-0000-0000-0000-0000000000d1","role":"authenticated"}';
select throws_ok($$ select count(*) from public.auth_attempts $$, '42501', null,
  'clients cannot read auth_attempts');
select throws_ok($$ select device_hash from public.known_devices $$, '42501', null,
  'device_hash is never readable by clients');
select is((select count(id)::int from public.known_devices), 1, 'the user sees their device');
select is((select count(*)::int from public.notifications where kind = 'login_alert'), 1,
  'the user sees their login alert');
set local "request.jwt.claims" = '{"sub":"00000000-0000-0000-0000-0000000000d2","role":"authenticated"}';
select is((select count(*)::int from public.notifications), 0, 'other users see no notifications');

reset role;
select * from finish();
rollback;
