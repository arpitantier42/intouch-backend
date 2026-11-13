-- pgTAP tests for the profile self-service RPCs: first-sign-in profile,
-- 2FA toggle, account deletion and the purge list. Run with: supabase test db
begin;
create extension if not exists pgtap with schema extensions;
select plan(11);

insert into auth.users (id) values ('00000000-0000-0000-0000-0000000000f1');

set local role authenticated;
set local "request.jwt.claims" = '{"sub":"00000000-0000-0000-0000-0000000000f1","role":"authenticated"}';

select is((select full_name::text from public.ensure_my_profile('Google Person', 'javascript:alert(1)')),
  'Google Person', 'the first call creates the profile with the provider name');
select is((select avatar_url from public.profiles where id = auth.uid()),
  null, 'a non-http avatar is dropped');
select is((select full_name::text from public.ensure_my_profile('Other', null)),
  'Google Person', 'a later call returns the existing profile unchanged');
select throws_ok($$ select public.set_two_factor(true) $$,
  'PT400', null, '2FA needs a verified phone');
select throws_ok($$ update public.profiles set two_factor_enabled = true where id = auth.uid() $$,
  '42501', null, 'clients cannot set two_factor_enabled directly');
select throws_ok($$ update public.profiles set deletion_requested_at = now() where id = auth.uid() $$,
  '42501', null, 'clients cannot set deletion_requested_at directly');
select throws_ok($$ select public.request_account_deletion('  ') $$,
  'PT400', null, 'a deletion needs a reason');
select ok((select deletion_requested_at is not null from public.request_account_deletion('Leaving')),
  'a deletion request starts the grace period');
select ok((select deletion_requested_at is null from public.cancel_account_deletion()),
  'cancelling clears it');
select throws_ok($$ select public.cancel_account_deletion() $$,
  'PT404', null, 'cancelling with nothing pending is 404');
select throws_ok($$ select * from public.accounts_due_for_purge(10) $$,
  '42501', null, 'clients cannot list accounts due for purge');

select * from finish();
rollback;
