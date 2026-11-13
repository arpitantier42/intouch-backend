-- pgTAP tests for admission, roster and role management RPCs.
-- Run with: supabase test db
begin;
create extension if not exists pgtap with schema extensions;
select plan(40);

-- o = owner of Gamma (public) and Member of Delta; a = admin; m = member;
-- p = pending admin-to-be; r = requester; d = owner of Delta.
insert into auth.users (id) values
  ('00000000-0000-0000-0000-0000000000a1'), ('00000000-0000-0000-0000-0000000000a2'),
  ('00000000-0000-0000-0000-0000000000a3'), ('00000000-0000-0000-0000-0000000000a4'),
  ('00000000-0000-0000-0000-0000000000a5'), ('00000000-0000-0000-0000-0000000000a6');
insert into public.profiles (id, full_name) values
  ('00000000-0000-0000-0000-0000000000a1', 'Olga Owner'), ('00000000-0000-0000-0000-0000000000a2', 'Adam Admin'),
  ('00000000-0000-0000-0000-0000000000a3', 'Mia Member'), ('00000000-0000-0000-0000-0000000000a4', 'Pat Pending'),
  ('00000000-0000-0000-0000-0000000000a5', 'Rui Requester'), ('00000000-0000-0000-0000-0000000000a6', 'Dee Delta');
insert into public.communities (id, handle, name, location_type, visibility, created_by, creation_key) values
  ('20000000-0000-0000-0000-000000000001', 'gamma', 'Gamma', 'online', 'public',
   '00000000-0000-0000-0000-0000000000a1', '30000000-0000-0000-0000-000000000001'),
  ('20000000-0000-0000-0000-000000000002', 'delta', 'Delta', 'online', 'public',
   '00000000-0000-0000-0000-0000000000a6', '30000000-0000-0000-0000-000000000002'),
  ('20000000-0000-0000-0000-000000000003', 'eps', 'Epsilon', 'online', 'private',
   '00000000-0000-0000-0000-0000000000a6', '30000000-0000-0000-0000-000000000003');
insert into public.memberships (community_id, user_id, role, status, via) values
  ('20000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-0000000000a1', 'owner', 'joined', 'direct'),
  ('20000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-0000000000a2', 'admin', 'joined', 'direct'),
  ('20000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-0000000000a3', 'member', 'joined', 'direct'),
  ('20000000-0000-0000-0000-000000000002', '00000000-0000-0000-0000-0000000000a6', 'owner', 'joined', 'direct'),
  ('20000000-0000-0000-0000-000000000002', '00000000-0000-0000-0000-0000000000a1', 'member', 'joined', 'direct'),
  ('20000000-0000-0000-0000-000000000002', '00000000-0000-0000-0000-0000000000a3', 'member', 'joined', 'direct'),
  ('20000000-0000-0000-0000-000000000003', '00000000-0000-0000-0000-0000000000a6', 'owner', 'joined', 'direct');
insert into public.community_questions (id, community_id, label, kind, required, position) values
  ('40000000-0000-0000-0000-000000000001', '20000000-0000-0000-0000-000000000001', 'Why join?', 'short_text', true, 0);

-- Schema rules.
select throws_ok($$ update public.memberships set role = 'superadmin'
  where user_id = '00000000-0000-0000-0000-0000000000a3' and community_id = '20000000-0000-0000-0000-000000000001' $$,
  '23514', null, 'role superadmin is rejected');
select throws_ok($$ update public.memberships set status = 'banned_x'
  where user_id = '00000000-0000-0000-0000-0000000000a3' and community_id = '20000000-0000-0000-0000-000000000001' $$,
  '23514', null, 'status banned_x is rejected');
select throws_ok($$ update public.memberships set role = 'owner'
  where user_id = '00000000-0000-0000-0000-0000000000a2' and community_id = '20000000-0000-0000-0000-000000000001' $$,
  '23505', null, 'a second owner outside a transfer is rejected');

set local role authenticated;

-- Rui (requester).
set local "request.jwt.claims" = '{"sub":"00000000-0000-0000-0000-0000000000a5","role":"authenticated"}';
select throws_ok($$ insert into public.memberships (community_id, user_id, role, status, via)
  values ('20000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-0000000000a5', 'member', 'joined', 'direct') $$,
  '42501', null, 'a user cannot insert a joined membership for themselves');
select throws_ok($$ select public.request_join('20000000-0000-0000-0000-000000000003') $$,
  'PT403', null, 'join request on a private community is rejected');
select throws_ok($$ select public.request_join('20000000-0000-0000-0000-000000000001',
  '[{"question_id":"40000000-0000-0000-0000-000000000001","value":"  "}]') $$,
  'PT400', null, 'a blank required answer blocks the request');
select is((select status || '/' || via from public.request_join('20000000-0000-0000-0000-000000000001',
  '[{"question_id":"40000000-0000-0000-0000-000000000001","value":"Love Go"}]')),
  'pending/request', 'request creates a pending membership via request');
select throws_ok($$ select public.request_join('20000000-0000-0000-0000-000000000001') $$,
  'PT409', null, 're-requesting while pending is rejected');
select ok((select count(*) = 1 from public.communities where id = '20000000-0000-0000-0000-000000000001'),
  'pending member sees the overview');
select ok(not public.is_joined('20000000-0000-0000-0000-000000000001'), 'pending member has no member access');
select is((select count(*)::int from public.memberships
  where community_id = '20000000-0000-0000-0000-000000000001' and user_id <> auth.uid()), 0,
  'pending member sees no roster');
select throws_ok($$ select public.list_members('20000000-0000-0000-0000-000000000001') $$,
  'PT403', null, 'pending member cannot open the admin roster');
select throws_ok($$ select public.review_join_request('20000000-0000-0000-0000-000000000001',
  '00000000-0000-0000-0000-0000000000a5', 'approve') $$, 'PT403', null, 'requester cannot review own request');

-- Mia (member): no admin actions.
set local "request.jwt.claims" = '{"sub":"00000000-0000-0000-0000-0000000000a3","role":"authenticated"}';
select throws_ok($$ select public.list_members('20000000-0000-0000-0000-000000000001') $$,
  'PT403', null, 'member opening the roster gets Admins only');
select ok(not public.can_invite('20000000-0000-0000-0000-000000000001'), 'member cannot invite with default settings');
select throws_ok($$ select public.review_join_request('20000000-0000-0000-0000-000000000001',
  '00000000-0000-0000-0000-0000000000a5', 'approve') $$, 'PT403', null, 'member cannot review');
select throws_ok($$ select public.remove_member('20000000-0000-0000-0000-000000000001',
  '00000000-0000-0000-0000-0000000000a2') $$, 'PT403', null, 'member cannot remove');

-- Adam (admin).
set local "request.jwt.claims" = '{"sub":"00000000-0000-0000-0000-0000000000a2","role":"authenticated"}';
select is((select jsonb_array_length(public.list_members('20000000-0000-0000-0000-000000000001', 'pending'))), 1,
  'admin sees the request under Requests');
select is((select public.list_members('20000000-0000-0000-0000-000000000001', 'pending') -> 0 -> 'answers' -> 0 ->> 'value'),
  'Love Go', 'admin sees the answers');
select is((select string_agg(e ->> 'role_label', ',') from
  jsonb_array_elements(public.list_members('20000000-0000-0000-0000-000000000001')) e),
  'Owner,Admin,Member', 'members tab lists role labels');
select throws_ok($$ select public.set_member_role('20000000-0000-0000-0000-000000000001',
  '00000000-0000-0000-0000-0000000000a3', 'admin') $$, 'PT403', null, 'admin cannot promote to admin');
select throws_ok($$ select public.remove_member('20000000-0000-0000-0000-000000000001',
  '00000000-0000-0000-0000-0000000000a2') $$, 'PT403', null, 'admin cannot remove self');
select throws_ok($$ select public.remove_member('20000000-0000-0000-0000-000000000001',
  '00000000-0000-0000-0000-0000000000a1') $$, 'PT403', null, 'admin cannot remove the owner');
select is((select status || '/' || via || '/' || reviewed_by::text from public.review_join_request(
  '20000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-0000000000a5', 'approve')),
  'joined/request/00000000-0000-0000-0000-0000000000a2', 'approved request joins via request');
select lives_ok($$ select public.remove_member('20000000-0000-0000-0000-000000000001',
  '00000000-0000-0000-0000-0000000000a5') $$, 'admin removes a member');

-- Rui after removal: access gone at once.
set local "request.jwt.claims" = '{"sub":"00000000-0000-0000-0000-0000000000a5","role":"authenticated"}';
select ok(not public.is_joined('20000000-0000-0000-0000-000000000001'), 'removed member loses access');
select is((select count(*)::int from public.memberships where community_id = '20000000-0000-0000-0000-000000000001'),
  0, 'removed member sees no roster');

-- Olga: owner of Gamma, member of Delta.
set local "request.jwt.claims" = '{"sub":"00000000-0000-0000-0000-0000000000a1","role":"authenticated"}';
select throws_ok($$ select public.set_member_role('20000000-0000-0000-0000-000000000002',
  '00000000-0000-0000-0000-0000000000a3', 'moderator') $$, 'PT403', null, 'owner of A cannot act as admin in B');
select throws_ok($$ select public.list_members('20000000-0000-0000-0000-000000000002') $$,
  'PT403', null, 'owner of A cannot read roster of B');
select throws_ok($$ select public.set_member_role('20000000-0000-0000-0000-000000000001',
  '00000000-0000-0000-0000-0000000000a3', 'owner') $$, 'PT400', null, 'owner role cannot be assigned');
select is((select role from public.set_member_role('20000000-0000-0000-0000-000000000001',
  '00000000-0000-0000-0000-0000000000a3', 'moderator')), 'moderator', 'owner promotes a member to moderator');
select throws_ok($$ select public.leave_community('20000000-0000-0000-0000-000000000001') $$,
  'PT409', null, 'owner cannot leave before transferring');
select throws_ok($$ select public.transfer_ownership('20000000-0000-0000-0000-000000000001',
  '00000000-0000-0000-0000-0000000000a4') $$, 'PT422', null, 'transfer target must be joined');
select is((select role from public.transfer_ownership('20000000-0000-0000-0000-000000000001',
  '00000000-0000-0000-0000-0000000000a3')), 'owner', 'transfer makes the target owner');
select is(public.community_role('20000000-0000-0000-0000-000000000001'), 'admin',
  'previous owner is admin after transfer');
select lives_ok($$ select public.leave_community('20000000-0000-0000-0000-000000000001') $$,
  'former owner can leave');
select ok(not public.is_joined('20000000-0000-0000-0000-000000000001'), 'leaving ends access');

-- Pat: withdraws a pending request.
set local "request.jwt.claims" = '{"sub":"00000000-0000-0000-0000-0000000000a4","role":"authenticated"}';
select lives_ok($$ select public.request_join('20000000-0000-0000-0000-000000000002') $$, 'pat requests Delta');
select lives_ok($$ select public.leave_community('20000000-0000-0000-0000-000000000002') $$, 'pat withdraws');
select is((select count(*)::int from public.memberships where user_id = auth.uid()), 0, 'request removed');
select throws_ok($$ select public.leave_community('20000000-0000-0000-0000-000000000002') $$,
  'PT404', null, 'nothing left to withdraw');

reset role;
select * from finish();
rollback;
