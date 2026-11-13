-- pgTAP tests for community create/update/overview/list/delete RPCs.
-- Run with: supabase test db
begin;
create extension if not exists pgtap with schema extensions;
select plan(12);

insert into auth.users (id) values
  ('00000000-0000-0000-0000-0000000000c1'), ('00000000-0000-0000-0000-0000000000c2');
insert into public.profiles (id, full_name) values
  ('00000000-0000-0000-0000-0000000000c1', 'Cara Creator'), ('00000000-0000-0000-0000-0000000000c2', 'Nik Nonmember');

select is(cardinality(public.community_cities()), 37, 'City/Area list has exactly 37 cities');
select is(jsonb_array_length(public.community_categories()), 7, 'seven categories');

set local role authenticated;
set local "request.jwt.claims" = '{"sub":"00000000-0000-0000-0000-0000000000c1","role":"authenticated"}';

select throws_ok($$select public.create_community('', 'online', gen_random_uuid())$$, 'PT400', null, 'blank name rejected');
select throws_ok($$select public.create_community('Club', 'place', gen_random_uuid())$$, 'PT400', null, 'place needs a city');
select lives_ok($$select public.create_community('Online Club', 'online', '40000000-0000-0000-0000-000000000001')$$, 'online needs no city');
select is((select via from public.memberships where user_id = '00000000-0000-0000-0000-0000000000c1'), 'direct', 'creator joins via direct');
select is((select visibility from public.communities where creation_key = '40000000-0000-0000-0000-000000000001'), 'private', 'private by default');
select is((select public.update_community(id, '{"category": "Sports & Play"}')->'interests'->>0
           from public.communities where creation_key = '40000000-0000-0000-0000-000000000001'),
          'Sports', 'primary interest auto-applied');
select throws_ok($$select public.update_community(id, '{"category": "Fitness & Gym", "interests": ["Knitting"]}')
                   from public.communities where creation_key = '40000000-0000-0000-0000-000000000001'$$,
                 'PT400', null, 'fixed category rejects custom interest');

select set_config('test.cid', (select id::text from public.communities where creation_key = '40000000-0000-0000-0000-000000000001'), true);
set local "request.jwt.claims" = '{"sub":"00000000-0000-0000-0000-0000000000c2","role":"authenticated"}';
select throws_ok($$select public.community_overview(current_setting('test.cid')::uuid)$$, 'PT403', null, 'private overview needs an invitation');
select is(jsonb_array_length(public.list_communities('search', 'Online Club')), 0, 'private not searchable');
select throws_ok($$select public.communities_due_for_purge(10)$$, '42501', null, 'purge list is service-only');

select * from finish();
rollback;
