-- Raises API (createRaise, getRaise, closeRaise, contributeToRaise) and the
-- webhook helpers (Razorpay capture, receipts, email bounces).
--
-- RPCs (SECURITY DEFINER, search_path = ''):
--   _inr(minor)               → '₹1,00,000' (Indian digit grouping; paise shown when non-zero)
--   _raise_json(m)            → raises_masked row + progress_percent (capped at 100),
--                               raised_label, goal_label, progress_label
--                               ('₹25,000 of ₹1,00,000 goal' | '₹25,000 raised'),
--                               is_open, status open|closed|ended, mine, post_id
--   create_raise(...)         → PT401/PT403 'Members only'/PT400 rules; inserts the raise,
--                               a feed post (kind raise, body = title) and its target
--   get_raise(id)             → PT404 unknown, not joined, or blocked either way
--   close_raise(id)           → PT404 / PT403 'Only the organiser can close this raise';
--                               keeps the first closed_at
--   start_contribution(id, amount, order_id)
--                             → PT404 / PT409 closed or past open_until / PT400 amount;
--                               with order_id null it only checks; else inserts a
--                               'created' contribution keyed by the gateway order id
--   capture_gateway_payment(key, raise, donor, amount) (service_role)
--                             → (contribution_id, applied) via capture_contribution;
--                               raise/donor come from the stored order row when present;
--                               null row when neither is known
--   contribution_receipt(id)  (service_role) → donor email + receipt fields
--   mark_invitations_failed_for_email(email) (service_role) → count of email
--                               invitations still 'sent' to that address marked failed
--   close_expired_raises(limit) (service_role) → ids closed (auto-close for the scheduler)

create or replace function public._inr(p_minor bigint)
returns text
language plpgsql immutable set search_path = ''
as $$
declare
  v_rupees text := (p_minor / 100)::text;
  v_paise int := (p_minor % 100)::int;
  v_head text;
  v_out text := '';
begin
  if p_minor is null then
    return null;
  end if;
  if length(v_rupees) > 3 then
    v_head := left(v_rupees, length(v_rupees) - 3);
    while length(v_head) > 2 loop
      v_out := ',' || right(v_head, 2) || v_out;
      v_head := left(v_head, length(v_head) - 2);
    end loop;
    v_rupees := v_head || v_out || ',' || right(v_rupees, 3);
  end if;
  return '₹' || v_rupees || case when v_paise <> 0 then '.' || lpad(v_paise::text, 2, '0') else '' end;
end;
$$;

create or replace function public._raise_json(m public.raises_masked)
returns jsonb
language sql stable security definer set search_path = ''
as $$
  select to_jsonb(m) || jsonb_build_object(
    'progress_percent', case when coalesce(m.goal_minor, 0) > 0
                             then least(100, floor(m.raised_minor * 100.0 / m.goal_minor))::int end,
    'raised_label', public._inr(m.raised_minor),
    'goal_label', public._inr(m.goal_minor),
    'progress_label', case when m.goal_minor is not null
                           then public._inr(m.raised_minor) || ' of ' || public._inr(m.goal_minor) || ' goal'
                           else public._inr(m.raised_minor) || ' raised' end,
    'is_open', m.closed_at is null and (m.open_until is null or m.open_until > now()),
    'status', case when m.closed_at is not null then 'closed'
                   when m.open_until is not null and m.open_until <= now() then 'ended'
                   else 'open' end,
    'mine', m.organiser_id = auth.uid(),
    'post_id', (select p.id from public.posts p
                where p.raise_id = m.id and p.deleted_at is null
                order by p.created_at limit 1))
$$;

-- The caller's view of one raise, or PT404.
create or replace function public._visible_raise(p_raise_id uuid)
returns public.raises_masked
language plpgsql stable security definer set search_path = ''
as $$
declare
  v public.raises_masked;
begin
  select * into v from public.raises_masked m where m.id = p_raise_id;
  if v.id is null
     or (v.organiser_id <> auth.uid() and public.blocked_between(auth.uid(), v.organiser_id)) then
    raise exception 'Raise not found' using errcode = 'PT404';
  end if;
  return v;
end;
$$;

create or replace function public.create_raise(
  p_community_id uuid,
  p_title text,
  p_description text default null,
  p_goal_minor bigint default null,
  p_open_until timestamptz default null,
  p_visibility text default null,
  p_invite_enabled boolean default false,
  p_receiver_name text default null,
  p_payout_mode text default null,
  p_upi_id text default null,
  p_account_number text default null,
  p_ifsc text default null,
  p_qr_url text default null)
returns jsonb
language plpgsql security definer set search_path = ''
as $$
declare
  v_uid uuid := auth.uid();
  v_title text := nullif(btrim(coalesce(p_title, '')), '');
  v_receiver text := nullif(btrim(coalesce(p_receiver_name, '')), '');
  v_mode text := lower(nullif(btrim(coalesce(p_payout_mode, '')), ''));
  v_vis text := lower(coalesce(nullif(btrim(p_visibility), ''), 'public'));
  v_upi text := nullif(btrim(coalesce(p_upi_id, '')), '');
  v_acct text := nullif(btrim(coalesce(p_account_number, '')), '');
  v_ifsc text := nullif(upper(btrim(coalesce(p_ifsc, ''))), '');
  v_qr text := nullif(btrim(coalesce(p_qr_url, '')), '');
  v_id uuid;
  v_post uuid;
begin
  if v_uid is null then
    raise exception 'Sign in to create a raise' using errcode = 'PT401';
  end if;
  if not public.is_joined(p_community_id)
     or not exists (select 1 from public.communities c where c.id = p_community_id and c.deleted_at is null) then
    raise exception 'Members only' using errcode = 'PT403';
  end if;
  if v_title is null then
    raise exception 'Add a title' using errcode = 'PT400';
  end if;
  if p_goal_minor is not null and p_goal_minor < 0 then
    raise exception 'Goal can''t be negative' using errcode = 'PT400';
  end if;
  if p_open_until is not null and p_open_until <= now() then
    raise exception 'Open until must be in the future' using errcode = 'PT400';
  end if;
  if v_vis not in ('public', 'private') then
    raise exception 'Visibility must be public or private' using errcode = 'PT400';
  end if;
  if v_receiver is null then
    raise exception 'Add who the money goes to' using errcode = 'PT400';
  end if;
  if v_mode is null or v_mode not in ('upi', 'bank') then
    raise exception 'Choose UPI or Bank' using errcode = 'PT400';
  end if;
  if v_mode = 'upi' then
    if v_upi is null then
      raise exception 'Add a UPI ID' using errcode = 'PT400';
    end if;
    v_acct := null;
    v_ifsc := null;
  else
    if v_acct is null then
      raise exception 'Add the account number' using errcode = 'PT400';
    end if;
    if v_ifsc is null then
      raise exception 'Add the IFSC code' using errcode = 'PT400';
    end if;
    if length(v_ifsc) <> 11 then
      raise exception 'IFSC must be 11 characters' using errcode = 'PT400';
    end if;
    v_upi := null;
  end if;
  if v_qr is not null and not public._is_http_url(v_qr) then
    raise exception 'QR image must be an http(s) link' using errcode = 'PT400';
  end if;

  insert into public.raises (community_id, organiser_id, title, description, goal_minor, open_until,
                             visibility, invite_enabled, receiver_name, payout_mode, upi_id,
                             account_number, ifsc, qr_url)
  values (p_community_id, v_uid, v_title, nullif(btrim(coalesce(p_description, '')), ''), p_goal_minor,
          p_open_until, v_vis, coalesce(p_invite_enabled, false), v_receiver, v_mode, v_upi,
          v_acct, v_ifsc, v_qr)
  returning id into v_id;

  -- The feed post: kind raise, body = the title.
  insert into public.posts (author_id, kind, body, raise_id)
  values (v_uid, 'raise', v_title, v_id)
  returning id into v_post;
  insert into public.post_targets (post_id, community_id) values (v_post, p_community_id);

  return public._raise_json(public._visible_raise(v_id));
end;
$$;

create or replace function public.get_raise(p_raise_id uuid)
returns jsonb
language sql stable security definer set search_path = ''
as $$
  select public._raise_json(public._visible_raise(p_raise_id))
$$;

create or replace function public.close_raise(p_raise_id uuid)
returns jsonb
language plpgsql security definer set search_path = ''
as $$
declare
  v public.raises_masked := public._visible_raise(p_raise_id);
begin
  if v.organiser_id <> auth.uid() then
    raise exception 'Only the organiser can close this raise' using errcode = 'PT403';
  end if;
  update public.raises r set closed_at = coalesce(r.closed_at, now()) where r.id = p_raise_id;
  return public._raise_json(public._visible_raise(p_raise_id));
end;
$$;

create or replace function public.start_contribution(p_raise_id uuid, p_amount_minor bigint, p_order_id text default null)
returns jsonb
language plpgsql security definer set search_path = ''
as $$
declare
  v public.raises_masked := public._visible_raise(p_raise_id);
  v_row public.contributions;
begin
  if v.closed_at is not null then
    raise exception 'This raise is closed' using errcode = 'PT409';
  end if;
  if v.open_until is not null and v.open_until <= now() then
    raise exception 'This raise is no longer open' using errcode = 'PT409';
  end if;
  if p_amount_minor is null or p_amount_minor < 100 then
    raise exception 'Enter an amount of at least ₹1' using errcode = 'PT400';
  end if;
  if p_order_id is null then
    return public._raise_json(v);
  end if;
  insert into public.contributions (raise_id, donor_id, amount_minor, gateway_payment_id, status)
  values (p_raise_id, auth.uid(), p_amount_minor, p_order_id, 'created')
  returning * into v_row;
  return to_jsonb(v_row);
end;
$$;

create or replace function public.capture_gateway_payment(
  p_key text, p_raise_id uuid, p_donor_id uuid, p_amount_minor bigint)
returns table (contribution_id uuid, applied boolean)
language plpgsql security definer set search_path = ''
as $$
declare
  v_raise uuid := p_raise_id;
  v_donor uuid := p_donor_id;
  v_amount bigint := p_amount_minor;
begin
  select c.raise_id, c.donor_id, c.amount_minor into v_raise, v_donor, v_amount
    from public.contributions c where c.gateway_payment_id = p_key;
  if not found then
    v_raise := p_raise_id;
    v_donor := p_donor_id;
    v_amount := p_amount_minor;
  end if;
  if v_raise is null or v_donor is null or coalesce(v_amount, 0) <= 0
     or not exists (select 1 from public.raises r where r.id = v_raise)
     or not exists (select 1 from public.profiles p where p.id = v_donor) then
    return;
  end if;
  return query select * from public.capture_contribution(v_raise, v_donor, v_amount, p_key);
end;
$$;

create or replace function public.contribution_receipt(p_contribution_id uuid)
returns jsonb
language sql stable security definer set search_path = ''
as $$
  select jsonb_build_object(
    'contribution_id', c.id, 'email', u.email, 'donor_name', p.full_name,
    'raise_id', r.id, 'raise_title', r.title, 'receiver_name', r.receiver_name,
    'amount_minor', c.amount_minor, 'amount_label', public._inr(c.amount_minor),
    'gateway_payment_id', c.gateway_payment_id, 'created_at', c.created_at)
  from public.contributions c
  join public.raises r on r.id = c.raise_id
  join public.profiles p on p.id = c.donor_id
  left join auth.users u on u.id = c.donor_id
  where c.id = p_contribution_id and c.status = 'captured'
$$;

create or replace function public.mark_invitations_failed_for_email(p_email text)
returns integer
language plpgsql security definer set search_path = ''
as $$
declare
  v_n integer;
begin
  update public.invitations set tracking_state = 'failed'
  where channel = 'email' and tracking_state = 'sent'
    and lower(invitee_email) = lower(btrim(p_email));
  get diagnostics v_n = row_count;
  return v_n;
end;
$$;

create or replace function public.close_expired_raises(p_limit integer default 500)
returns setof uuid
language sql security definer set search_path = ''
as $$
  update public.raises r set closed_at = r.open_until
  where r.id in (select x.id from public.raises x
                 where x.closed_at is null and x.open_until is not null and x.open_until <= now()
                 limit greatest(coalesce(p_limit, 500), 1))
  returning r.id
$$;

revoke all on function
  public._raise_json(public.raises_masked),
  public._visible_raise(uuid),
  public.create_raise(uuid, text, text, bigint, timestamptz, text, boolean, text, text, text, text, text, text),
  public.get_raise(uuid),
  public.close_raise(uuid),
  public.start_contribution(uuid, bigint, text),
  public.capture_gateway_payment(text, uuid, uuid, bigint),
  public.contribution_receipt(uuid),
  public.mark_invitations_failed_for_email(text),
  public.close_expired_raises(integer)
  from public, anon, authenticated;

grant execute on function
  public.create_raise(uuid, text, text, bigint, timestamptz, text, boolean, text, text, text, text, text, text),
  public.get_raise(uuid),
  public.close_raise(uuid),
  public.start_contribution(uuid, bigint, text)
  to authenticated, service_role;
grant execute on function
  public._raise_json(public.raises_masked),
  public._visible_raise(uuid),
  public.capture_gateway_payment(text, uuid, uuid, bigint),
  public.contribution_receipt(uuid),
  public.mark_invitations_failed_for_email(text),
  public.close_expired_raises(integer)
  to service_role;
