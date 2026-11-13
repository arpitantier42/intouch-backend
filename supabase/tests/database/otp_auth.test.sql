-- pgTAP tests for ensure_profile: the profile created on first OTP
-- verification. Run with: supabase test db
begin;
create extension if not exists pgtap with schema extensions;
select plan(6);

insert into auth.users (id) values
  ('00000000-0000-0000-0000-0000000000e1'),
  ('00000000-0000-0000-0000-0000000000e2');

set local role service_role;
select is((select onboarding_complete from public.ensure_profile('00000000-0000-0000-0000-0000000000e1', '  Asha Rao ')),
  false, 'a new profile starts with onboarding incomplete');
select is((select full_name::text from public.profiles where id = '00000000-0000-0000-0000-0000000000e1'),
  'Asha Rao', 'the name is trimmed');
select is((select full_name::text from public.ensure_profile('00000000-0000-0000-0000-0000000000e1', 'Other Name')),
  'Asha Rao', 'a second call returns the existing profile unchanged');
select is((select full_name::text from public.ensure_profile('00000000-0000-0000-0000-0000000000e2', 'A')),
  'New member', 'an invalid name falls back to a placeholder');
select throws_ok($$ select public.ensure_profile('00000000-0000-0000-0000-0000000000ff', 'Ghost User') $$,
  'PT404', null, 'an unknown auth user is rejected');

reset role;
set local role authenticated;
set local "request.jwt.claims" = '{"sub":"00000000-0000-0000-0000-0000000000e1","role":"authenticated"}';
select throws_ok($$ select public.ensure_profile('00000000-0000-0000-0000-0000000000e1', 'Asha Rao') $$,
  '42501', null, 'clients cannot call ensure_profile');

select * from finish();
rollback;
