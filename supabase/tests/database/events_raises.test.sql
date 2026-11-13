-- pgTAP tests for events, RSVPs, reminders, raises and contributions.
-- Run with: supabase test db
begin;
create extension if not exists pgtap with schema extensions;
select plan(11);

insert into auth.users (id) values
  ('00000000-0000-0000-0000-0000000000a3'),
  ('00000000-0000-0000-0000-0000000000b3'),
  ('00000000-0000-0000-0000-0000000000c3');
insert into public.profiles (id, full_name) values
  ('00000000-0000-0000-0000-0000000000a3', 'Alice A'),
  ('00000000-0000-0000-0000-0000000000b3', 'Bob B'),
  ('00000000-0000-0000-0000-0000000000c3', 'Carol C');

-- Alice owns community E; Bob and Carol are joined members.
set local role authenticated;
set local "request.jwt.claims" = '{"sub":"00000000-0000-0000-0000-0000000000a3","role":"authenticated"}';
select public.create_community('Events E', 'online', '20000000-0000-0000-0000-000000000003');
reset role;
insert into public.memberships (community_id, user_id, role, status, via)
select c.id, u.id, 'member', 'joined', 'direct'
from public.communities c,
     (values ('00000000-0000-0000-0000-0000000000b3'::uuid), ('00000000-0000-0000-0000-0000000000c3'::uuid)) u (id)
where c.name = 'Events E';

select throws_ok(
  $$ insert into public.events (community_id, creator_id, title, starts_at, ends_at, format, venue, is_paid, price_minor)
     select c.id, '00000000-0000-0000-0000-0000000000a3', 'Paid', now() + interval '1 day',
            now() + interval '1 day 2 hours', 'in_person', 'Hall', true, 1000
     from public.communities c where c.name = 'Events E' $$,
  '23514', null, 'a paid event without a payment link is blocked');

select throws_ok(
  $$ insert into public.events (community_id, creator_id, title, starts_at, ends_at, format, venue, is_paid, price_minor, payment_url)
     select c.id, '00000000-0000-0000-0000-0000000000a3', 'Paid', now() + interval '1 day',
            now() + interval '1 day 2 hours', 'in_person', 'Hall', true, 1000, 'ftp://pay.example.com'
     from public.communities c where c.name = 'Events E' $$,
  '23514', null, 'a non-http(s) payment link is rejected');

-- A one-seat event.
insert into public.events (id, community_id, creator_id, title, starts_at, ends_at, format, venue, seat_cap)
select '50000000-0000-0000-0000-000000000001', c.id, '00000000-0000-0000-0000-0000000000a3', 'Meetup',
       now() + interval '3 days', now() + interval '3 days 2 hours', 'in_person', 'Hall', 1
from public.communities c where c.name = 'Events E';

insert into public.rsvps (event_id, user_id, status, remind_push, remind_email) values
  ('50000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-0000000000b3', 'going', true, false);

select is((select count(*)::int from public.reminders
           where user_id = '00000000-0000-0000-0000-0000000000b3' and channel = 'push'
             and send_at = (select starts_at - interval '1 hour' from public.events
                            where id = '50000000-0000-0000-0000-000000000001')),
          1, 'push on schedules a push 1 hour before start');
select is((select count(*)::int from public.reminders
           where user_id = '00000000-0000-0000-0000-0000000000b3' and channel = 'email'),
          0, 'email off schedules no day-of email');

select throws_ok(
  $$ insert into public.rsvps (event_id, user_id, status)
     values ('50000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-0000000000c3', 'going') $$,
  'PT409', null, 'a full event rejects another going RSVP');

set local role authenticated;
set local "request.jwt.claims" = '{"sub":"00000000-0000-0000-0000-0000000000c3","role":"authenticated"}';
select is(public.seats_left('50000000-0000-0000-0000-000000000001'), 0, 'no seats left');
reset role;

-- A bank raise organised by Bob.
insert into public.raises (id, community_id, organiser_id, title, receiver_name, payout_mode, account_number, ifsc)
select '60000000-0000-0000-0000-000000000001', c.id, '00000000-0000-0000-0000-0000000000b3', 'Fund',
       'Trust', 'bank', '000123456789', 'HDFC0001234'
from public.communities c where c.name = 'Events E';

select throws_ok(
  $$ insert into public.raises (community_id, organiser_id, title, receiver_name, payout_mode, account_number)
     select c.id, '00000000-0000-0000-0000-0000000000b3', 'Fund', 'Trust', 'bank', '000123456789'
     from public.communities c where c.name = 'Events E' $$,
  '23514', null, 'bank mode without IFSC is rejected');

set local role authenticated;
set local "request.jwt.claims" = '{"sub":"00000000-0000-0000-0000-0000000000c3","role":"authenticated"}';
select is((select coalesce(account_number, '-') || ':' || account_last4 from public.raises_masked
           where id = '60000000-0000-0000-0000-000000000001'),
          '-:6789', 'a member sees only the last 4 account digits');
select throws_ok($$ select account_number from public.raises $$, '42501', null,
                 'the raises base table is denied to members');
reset role;

select is((select applied from public.capture_contribution(
             '60000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-0000000000c3', 5000, 'pay_T1')),
          true, 'the first capture applies');
select is((select (not applied) and r.raised_minor = 5000
           from public.capture_contribution(
             '60000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-0000000000c3', 5000, 'pay_T1'),
           public.raises r where r.id = '60000000-0000-0000-0000-000000000001'),
          true, 'a duplicate webhook does not add again');

select * from finish();
rollback;
