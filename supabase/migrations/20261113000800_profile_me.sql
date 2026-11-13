-- Profile self-service: first-sign-in profile creation (Google), SMS 2FA
-- toggle and account deletion with a 14-day grace period.
--
-- Clients may update their self-service profile columns directly (column
-- grants in the identity migration). The columns below are server-set and
-- change only through these SECURITY DEFINER functions:
--   two_factor_enabled, phone            -> set_two_factor(enabled)
--   deletion_requested_at, deletion_reason -> request/cancel_account_deletion

-- Creates the caller's profile on the first authenticated request (first
-- Google sign-in creates the Auth user but no profile) and returns it.
-- Idempotent: an existing profile is returned unchanged. A name that is not
-- 2-80 characters falls back to 'New member' (onboarding asks again).
create or replace function public.ensure_my_profile(p_full_name text default null,
                                                    p_avatar_url text default null)
returns public.profiles
language plpgsql security definer set search_path = ''
as $$
declare
  v_uid uuid := auth.uid();
  v_name text := btrim(coalesce(p_full_name, ''));
  v_avatar text := nullif(btrim(coalesce(p_avatar_url, '')), '');
  v_profile public.profiles;
begin
  if v_uid is null then
    raise exception 'Sign in to continue.' using errcode = 'PT401';
  end if;
  select * into v_profile from public.profiles p where p.id = v_uid;
  if found then
    return v_profile;
  end if;
  if not exists (select 1 from auth.users u where u.id = v_uid) then
    raise exception 'Unknown user.' using errcode = 'PT401';
  end if;
  if char_length(v_name) not between 2 and 80 then
    v_name := 'New member';
  end if;
  if v_avatar is not null and v_avatar !~ '^https?://' then
    v_avatar := null;
  end if;
  insert into public.profiles (id, full_name, avatar_url)
  values (v_uid, v_name, v_avatar)
  on conflict (id) do nothing;
  select * into v_profile from public.profiles p where p.id = v_uid;
  return v_profile;
end;
$$;

revoke execute on function public.ensure_my_profile(text, text) from public, anon;
grant execute on function public.ensure_my_profile(text, text) to authenticated, service_role;

-- Turns SMS two-factor on or off for the caller. Turning it on needs a
-- verified phone: the profile's phone, else the phone Supabase Auth has
-- confirmed for this user (copied to the profile). PT400 otherwise.
create or replace function public.set_two_factor(p_enabled boolean)
returns public.profiles
language plpgsql security definer set search_path = ''
as $$
declare
  v_uid uuid := auth.uid();
  v_phone text;
  v_profile public.profiles;
begin
  select * into v_profile from public.profiles p where p.id = v_uid for update;
  if not found then
    raise exception 'Profile not found.' using errcode = 'PT404';
  end if;
  if not coalesce(p_enabled, false) then
    update public.profiles set two_factor_enabled = false where id = v_uid
      returning * into v_profile;
    return v_profile;
  end if;
  v_phone := v_profile.phone;
  if v_phone is null then
    select case when u.phone like '+%' then u.phone else '+' || u.phone end
      into v_phone
      from auth.users u
     where u.id = v_uid and u.phone_confirmed_at is not null
       and coalesce(u.phone, '') <> '';
  end if;
  if v_phone is null then
    raise exception 'Verify a phone number before turning on two-factor authentication.'
      using errcode = 'PT400';
  end if;
  update public.profiles set phone = v_phone, two_factor_enabled = true where id = v_uid
    returning * into v_profile;
  return v_profile;
end;
$$;

revoke execute on function public.set_two_factor(boolean) from public, anon;
grant execute on function public.set_two_factor(boolean) to authenticated, service_role;

-- Schedules the caller's account for deletion (purge 14 days later).
-- Requires a reason; repeating keeps the original request time and
-- updates the reason.
create or replace function public.request_account_deletion(p_reason text)
returns public.profiles
language plpgsql security definer set search_path = ''
as $$
declare
  v_reason text := btrim(coalesce(p_reason, ''));
  v_profile public.profiles;
begin
  if v_reason = '' then
    raise exception 'Tell us why you are leaving.' using errcode = 'PT400';
  end if;
  if char_length(v_reason) > 500 then
    raise exception 'Reason must be 500 characters or fewer.' using errcode = 'PT400';
  end if;
  update public.profiles
     set deletion_requested_at = coalesce(deletion_requested_at, now()),
         deletion_reason = v_reason
   where id = auth.uid()
  returning * into v_profile;
  if not found then
    raise exception 'Profile not found.' using errcode = 'PT404';
  end if;
  return v_profile;
end;
$$;

revoke execute on function public.request_account_deletion(text) from public, anon;
grant execute on function public.request_account_deletion(text) to authenticated, service_role;

-- Cancels the caller's pending deletion. PT404 when none is pending.
create or replace function public.cancel_account_deletion()
returns public.profiles
language plpgsql security definer set search_path = ''
as $$
declare
  v_profile public.profiles;
begin
  update public.profiles
     set deletion_requested_at = null, deletion_reason = null
   where id = auth.uid() and deletion_requested_at is not null
  returning * into v_profile;
  if not found then
    raise exception 'No pending deletion.' using errcode = 'PT404';
  end if;
  return v_profile;
end;
$$;

revoke execute on function public.cancel_account_deletion() from public, anon;
grant execute on function public.cancel_account_deletion() to authenticated, service_role;

-- Scheduler: users whose 14-day grace period is over, oldest first. The
-- scheduler deletes each through the Supabase Auth admin API (the profile
-- and its rows cascade from auth.users). Users who still own a live
-- community are skipped until ownership is transferred.
create or replace function public.accounts_due_for_purge(p_limit integer default 100)
returns table (user_id uuid, deletion_requested_at timestamptz)
language sql stable security definer set search_path = ''
as $$
  select p.id, p.deletion_requested_at
    from public.profiles p
   where p.deletion_requested_at <= now() - interval '14 days'
     and not exists (
       select 1 from public.memberships m
         join public.communities c on c.id = m.community_id
        where m.user_id = p.id and m.role = 'owner' and c.deleted_at is null)
   order by p.deletion_requested_at
   limit greatest(coalesce(p_limit, 100), 0)
$$;

revoke execute on function public.accounts_due_for_purge(integer) from public, anon, authenticated;
grant execute on function public.accounts_due_for_purge(integer) to service_role;
