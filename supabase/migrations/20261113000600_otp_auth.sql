-- OTP sign-up / verify support: the profile is created once Supabase Auth
-- has verified the code (never at sign-up).

-- Creates the caller's profile on first verification and returns it.
-- Idempotent: an existing profile is returned unchanged. An invalid or
-- missing name falls back to 'New member' (onboarding asks again).
-- service_role only.
create or replace function public.ensure_profile(p_user_id uuid, p_full_name text)
returns public.profiles
language plpgsql security definer set search_path = ''
as $$
declare
  v_name text := btrim(coalesce(p_full_name, ''));
  v_profile public.profiles;
begin
  if not exists (select 1 from auth.users u where u.id = p_user_id) then
    raise exception 'Unknown user.' using errcode = 'PT404';
  end if;
  if char_length(v_name) not between 2 and 80 then
    v_name := 'New member';
  end if;
  insert into public.profiles (id, full_name)
  values (p_user_id, v_name)
  on conflict (id) do nothing;
  select * into v_profile from public.profiles p where p.id = p_user_id;
  return v_profile;
end;
$$;

revoke execute on function public.ensure_profile(uuid, text) from public, anon, authenticated;
grant execute on function public.ensure_profile(uuid, text) to service_role;
