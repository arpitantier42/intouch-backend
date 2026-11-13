-- Invitations API: issuing (permission-gated, one random 128-bit code per
-- recipient), revoking, and marking a failed email delivery. Opening and
-- accepting (open_invitation, accept_invitation) live in the identity
-- migration.

-- Issues invitations for p_community_id. Callers must be allowed by the
-- community's invite_permission (PT403 otherwise, including non-members).
--   link | whatsapp: one invitation (recipients ignored)
--   email:           one per distinct address in p_emails
--   in_app:          one per distinct user in p_user_ids
-- PT400: unknown channel, no recipients, > 100 recipients, malformed email,
-- past expiry, max_uses < 1, invitee is the caller or already joined.
-- PT404: unknown in-app user. Returns a jsonb array of the invitation rows.
create or replace function public.create_invitations(
  p_community_id uuid,
  p_channel text,
  p_emails text[] default null,
  p_user_ids uuid[] default null,
  p_expires_at timestamptz default null,
  p_max_uses integer default null
)
returns jsonb
language plpgsql security definer set search_path = ''
as $$
declare
  v_uid uuid := auth.uid();
  v_expires timestamptz := coalesce(p_expires_at, now() + interval '14 days');
  v_max integer := coalesce(p_max_uses, 1);
  v_my_email text;
  v_email text;
  v_user uuid;
  v_ids uuid[] := '{}';
  v_id uuid;
begin
  if v_uid is null then
    raise exception 'Sign in to invite members.' using errcode = 'PT401';
  end if;
  if not public.can_invite(p_community_id) then
    raise exception 'Your role cannot invite members to this community.' using errcode = 'PT403';
  end if;
  if p_channel is null or p_channel not in ('link', 'whatsapp', 'email', 'in_app') then
    raise exception 'Channel must be link, whatsapp, email or in_app.' using errcode = 'PT400';
  end if;
  if v_expires <= now() then
    raise exception 'Expiry must be in the future.' using errcode = 'PT400';
  end if;
  if v_max < 1 then
    raise exception 'Max uses must be at least 1.' using errcode = 'PT400';
  end if;

  if p_channel in ('link', 'whatsapp') then
    insert into public.invitations (community_id, channel, created_by, expires_at, max_uses)
    values (p_community_id, p_channel, v_uid, v_expires, v_max)
    returning id into v_id;
    v_ids := array[v_id];

  elsif p_channel = 'email' then
    if coalesce(cardinality(p_emails), 0) = 0 then
      raise exception 'Add at least one email address.' using errcode = 'PT400';
    end if;
    if cardinality(p_emails) > 100 then
      raise exception 'Invite at most 100 people at a time.' using errcode = 'PT400';
    end if;
    select u.email into v_my_email from auth.users u where u.id = v_uid;
    for v_email in
      select min(trim(e)) from unnest(p_emails) e group by lower(trim(e)) order by min(trim(e))
    loop
      if v_email is null or length(v_email) > 255
         or v_email !~ '^[^[:space:]@<>]+@[^[:space:]@<>]+\.[^[:space:]@<>]+$' then
        raise exception '% is not a valid email address.', coalesce(v_email, 'An address') using errcode = 'PT400';
      end if;
      if lower(v_email) = lower(v_my_email) then
        raise exception 'You can''t invite yourself.' using errcode = 'PT400';
      end if;
      if exists (
        select 1 from auth.users u
        join public.memberships m on m.user_id = u.id
        where lower(u.email) = lower(v_email)
          and m.community_id = p_community_id and m.status = 'joined'
      ) then
        raise exception '% is already a member.', v_email using errcode = 'PT400';
      end if;
      insert into public.invitations (community_id, channel, invitee_email, created_by, expires_at, max_uses)
      values (p_community_id, 'email', v_email, v_uid, v_expires, v_max)
      returning id into v_id;
      v_ids := v_ids || v_id;
    end loop;

  else -- in_app
    if coalesce(cardinality(p_user_ids), 0) = 0 then
      raise exception 'Choose at least one person to invite.' using errcode = 'PT400';
    end if;
    if cardinality(p_user_ids) > 100 then
      raise exception 'Invite at most 100 people at a time.' using errcode = 'PT400';
    end if;
    for v_user in select distinct u from unnest(p_user_ids) u order by 1 loop
      if v_user is null or v_user = v_uid then
        raise exception 'You can''t invite yourself.' using errcode = 'PT400';
      end if;
      if not exists (select 1 from public.profiles p where p.id = v_user) then
        raise exception 'User not found.' using errcode = 'PT404';
      end if;
      if exists (
        select 1 from public.memberships m
        where m.community_id = p_community_id and m.user_id = v_user and m.status = 'joined'
      ) then
        raise exception 'This person is already a member.' using errcode = 'PT400';
      end if;
      insert into public.invitations (community_id, channel, invitee_user_id, created_by, expires_at, max_uses)
      values (p_community_id, 'in_app', v_user, v_uid, v_expires, v_max)
      returning id into v_id;
      v_ids := v_ids || v_id;
    end loop;
  end if;

  return (
    select jsonb_agg(to_jsonb(i) order by array_position(v_ids, i.id))
    from public.invitations i where i.id = any (v_ids)
  );
end
$$;

-- Revokes an invitation (idempotent: the first revoked_at is kept).
-- PT403 unless the caller may invite; PT404 unknown code for this community.
create or replace function public.revoke_invitation(p_community_id uuid, p_code text)
returns public.invitations
language plpgsql security definer set search_path = ''
as $$
declare
  v_inv public.invitations;
begin
  if not public.can_invite(p_community_id) then
    raise exception 'Your role cannot manage invitations for this community.' using errcode = 'PT403';
  end if;
  update public.invitations
  set revoked_at = coalesce(revoked_at, now())
  where community_id = p_community_id and code = p_code
  returning * into v_inv;
  if not found then
    raise exception 'Invitation not found.' using errcode = 'PT404';
  end if;
  return v_inv;
end
$$;

-- Marks an email invitation whose delivery failed or bounced (service only:
-- the API after a provider error, the email bounce webhook). Only an email
-- invitation not yet opened or accepted changes; returns whether it did.
create or replace function public.mark_invitation_failed(p_invitation_id uuid)
returns boolean
language plpgsql security definer set search_path = ''
as $$
begin
  update public.invitations set tracking_state = 'failed'
  where id = p_invitation_id and channel = 'email' and tracking_state = 'sent';
  return found;
end
$$;

revoke all on function
  public.create_invitations(uuid, text, text[], uuid[], timestamptz, integer),
  public.revoke_invitation(uuid, text),
  public.mark_invitation_failed(uuid)
  from public, anon, authenticated;

grant execute on function
  public.create_invitations(uuid, text, text[], uuid[], timestamptz, integer),
  public.revoke_invitation(uuid, text)
  to authenticated, service_role;
grant execute on function public.mark_invitation_failed(uuid) to service_role;
