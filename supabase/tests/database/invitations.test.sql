-- pgTAP tests for the invitation RPCs (create, revoke, mark failed).
-- Run with: supabase test db
begin;
create extension if not exists pgtap with schema extensions;
select plan(12);

-- o = owner; m = moderator; x = outsider.
insert into auth.users (id, email) values
  ('00000000-0000-0000-0000-0000000000b1', 'o@example.com'),
  ('00000000-0000-0000-0000-0000000000b2', 'm@example.com'),
  ('00000000-0000-0000-0000-0000000000b3', 'x@example.com');
insert into public.profiles (id, full_name) values
  ('00000000-0000-0000-0000-0000000000b1', 'Olga Owner'),
  ('00000000-0000-0000-0000-0000000000b2', 'Mo Moderator'),
  ('00000000-0000-0000-0000-0000000000b3', 'Xena Outsider');
insert into public.communities (id, handle, name, location_type, visibility, created_by, creation_key) values
  ('21000000-0000-0000-0000-000000000001', 'inv', 'Inv', 'online', 'private',
   '00000000-0000-0000-0000-0000000000b1', '31000000-0000-0000-0000-000000000001');
insert into public.memberships (community_id, user_id, role, status, via) values
  ('21000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-0000000000b1', 'owner', 'joined', 'direct'),
  ('21000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-0000000000b2', 'moderator', 'joined', 'direct');

set local role authenticated;

-- Moderator under the default invite_permission (admins).
set local "request.jwt.claims" = '{"sub":"00000000-0000-0000-0000-0000000000b2","role":"authenticated"}';
select throws_ok($$select public.create_invitations('21000000-0000-0000-0000-000000000001', 'link')$$,
  'PT403', null, 'moderator cannot invite by default');
select throws_ok($$select public.revoke_invitation('21000000-0000-0000-0000-000000000001', 'x')$$,
  'PT403', null, 'moderator cannot revoke by default');
select throws_ok($$select public.mark_invitation_failed('21000000-0000-0000-0000-000000000001')$$,
  '42501', null, 'clients cannot mark invitations failed');

-- Owner.
set local "request.jwt.claims" = '{"sub":"00000000-0000-0000-0000-0000000000b1","role":"authenticated"}';
select is(jsonb_array_length(public.create_invitations('21000000-0000-0000-0000-000000000001', 'in_app',
  null, array['00000000-0000-0000-0000-0000000000b3']::uuid[])), 1, 'owner invites an outsider in-app');
select throws_ok($$select public.create_invitations('21000000-0000-0000-0000-000000000001', 'in_app', null,
  array['00000000-0000-0000-0000-0000000000b2']::uuid[])$$, 'PT400', null, 'existing member rejected');
select throws_ok($$select public.create_invitations('21000000-0000-0000-0000-000000000001', 'email',
  array['O@example.com'])$$, 'PT400', null, 'own email rejected');
select throws_ok($$select public.create_invitations('21000000-0000-0000-0000-000000000001', 'email',
  array['ok@example.com', 'not-an-email'])$$, 'PT400', null, 'malformed email rejected');
select is(jsonb_array_length(public.create_invitations('21000000-0000-0000-0000-000000000001', 'email',
  array['new@example.com', 'NEW@example.com ', 'two@example.com'])), 2, 'addresses are deduplicated');
select throws_ok($$select public.revoke_invitation('21000000-0000-0000-0000-000000000001', 'nope')$$,
  'PT404', null, 'unknown code');

reset role;
select ok(public.mark_invitation_failed(
  (select id from public.invitations where invitee_email = 'two@example.com')), 'email invite marked failed');
select is((select tracking_state from public.invitations where invitee_email = 'two@example.com'),
  'failed', 'tracking state is failed');
select ok(not public.mark_invitation_failed(
  (select id from public.invitations where channel = 'in_app')), 'non-email invitation unchanged');

select * from finish();
rollback;
