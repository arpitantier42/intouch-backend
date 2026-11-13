-- pgTAP tests for session revocation (sign out local/others, password
-- change). Run with: supabase test db
begin;
create extension if not exists pgtap with schema extensions;
select plan(9);

insert into auth.users (id) values ('00000000-0000-0000-0000-0000000000f1');

set local role service_role;
select is((select full_name::text from public.ensure_profile('00000000-0000-0000-0000-0000000000f1', 'Sam Lee')),
  'Sam Lee', 'profile created');
select is(public.session_revoked('00000000-0000-0000-0000-0000000000f1', '00000000-0000-0000-0000-00000000aaa1', now()),
  false, 'a fresh session is valid');

select public.revoke_session('00000000-0000-0000-0000-0000000000f1', '00000000-0000-0000-0000-00000000aaa1');
select is(public.session_revoked('00000000-0000-0000-0000-0000000000f1', '00000000-0000-0000-0000-00000000aaa1', now()),
  true, 'a signed-out session is rejected');
select is(public.session_revoked('00000000-0000-0000-0000-0000000000f1', '00000000-0000-0000-0000-00000000aaa2', now() - interval '1 minute'),
  false, 'other sessions are unaffected by a local sign out');

select public.revoke_other_sessions('00000000-0000-0000-0000-0000000000f1', '00000000-0000-0000-0000-00000000aaa3');
select is(public.session_revoked('00000000-0000-0000-0000-0000000000f1', '00000000-0000-0000-0000-00000000aaa2', now() - interval '1 minute'),
  true, 'sign out others rejects older tokens of other sessions');
select is(public.session_revoked('00000000-0000-0000-0000-0000000000f1', '00000000-0000-0000-0000-00000000aaa3', now() - interval '1 minute'),
  false, 'the kept session stays valid');
select is(public.session_revoked('00000000-0000-0000-0000-0000000000f1', '00000000-0000-0000-0000-00000000aaa4', now() + interval '1 minute'),
  false, 'sessions issued after the cutoff are valid');

reset role;
set local role authenticated;
set local "request.jwt.claims" = '{"sub":"00000000-0000-0000-0000-0000000000f1","role":"authenticated"}';
select throws_ok($$ select public.session_revoked('00000000-0000-0000-0000-0000000000f1', null, now()) $$,
  '42501', null, 'clients cannot call session_revoked');
select throws_ok($$ select count(*) from public.revoked_sessions $$,
  '42501', null, 'clients cannot read revoked_sessions');

select * from finish();
rollback;
