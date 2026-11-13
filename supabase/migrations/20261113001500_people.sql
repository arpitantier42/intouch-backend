-- People search and member profile API (listPeople, getMemberProfile).
-- Mentorship (request_mentorship) and follow (follow_user, follow_counts)
-- RPCs live in the social graph migration.

-- True when the caller and p_user_id are both joined to at least one live
-- community (or to p_community_id when it is given).
create or replace function public._shares_live_community(p_me uuid, p_user_id uuid, p_community_id uuid default null)
returns boolean
language sql stable security definer set search_path = ''
as $$
  select exists (
    select 1
    from public.memberships a
    join public.memberships b on b.community_id = a.community_id
    join public.communities c on c.id = a.community_id and c.deleted_at is null
    where a.user_id = p_me and a.status = 'joined'
      and b.user_id = p_user_id and b.status = 'joined'
      and (p_community_id is null or a.community_id = p_community_id))
$$;

-- Ids of the users connected (state connected) to p_user_id.
create or replace function public._connected_ids(p_user_id uuid)
returns setof uuid
language sql stable security definer set search_path = ''
as $$
  select case when c.requester_id = p_user_id then c.addressee_id else c.requester_id end
  from public.connections c
  where c.state = 'connected' and p_user_id in (c.requester_id, c.addressee_id)
$$;

-- One people query for the member directory, My Connections, People you may
-- know, the new-chat / share picker and the invite sheet's In-app users tab.
--   p_mode connections  the caller's connections (scoped to joined members of
--                       p_community_id when given)
--          members      joined members of p_community_id (required)
--          suggested    not connected or pending, sharing a live community
--                       (p_community_id when given), ranked by shared
--                       communities, city, company and skills
--          invitable    people who are not joined to p_community_id
--                       (required; caller must be allowed to invite)
--   p_q      name, username, headline, company or a skill (case-insensitive)
--   p_filter everyone|same_city|near_me|same_company — from the caller's
--            profile city/company (no GPS); no city/company → no rows.
--            City filters skip people who hide their city.
-- Always excludes the caller and blocks either way; excludes
-- appear_in_search = false in every mode except connections.
-- Returns a jsonb array of person cards (city null when hidden) with
-- `connection_state`. PT400 bad mode/filter or missing community;
-- PT403 not joined (or cannot invite for invitable).
create or replace function public.search_people(
    p_mode text, p_community_id uuid default null, p_q text default null,
    p_filter text default null, p_limit integer default 50)
returns jsonb
language plpgsql stable security definer set search_path = ''
as $$
declare
  v_me uuid := auth.uid();
  v_mode text := lower(btrim(coalesce(p_mode, '')));
  v_filter text := coalesce(nullif(lower(btrim(coalesce(p_filter, ''))), ''), 'everyone');
  v_q text := nullif(btrim(coalesce(p_q, '')), '');
  v_pat text;
  v_city text;
  v_company text;
  v_skills jsonb;
  v_limit integer := least(greatest(coalesce(p_limit, 50), 1), 100);
begin
  if v_me is null then
    raise exception 'Sign in required.' using errcode = 'PT401';
  end if;
  if v_mode not in ('connections', 'members', 'suggested', 'invitable') then
    raise exception 'Mode must be connections, members, suggested or invitable.' using errcode = 'PT400';
  end if;
  if v_filter not in ('everyone', 'same_city', 'near_me', 'same_company') then
    raise exception 'Filter must be everyone, same_city, near_me or same_company.' using errcode = 'PT400';
  end if;
  if v_mode in ('members', 'invitable') and p_community_id is null then
    raise exception 'Choose a community.' using errcode = 'PT400';
  end if;
  if p_community_id is not null then
    if v_mode = 'invitable' then
      if not public._joined_member(p_community_id, v_me) or not public.can_invite(p_community_id) then
        raise exception 'You cannot invite people to this community.' using errcode = 'PT403';
      end if;
    elsif not public._joined_member(p_community_id, v_me) then
      raise exception 'Members only.' using errcode = 'PT403';
    end if;
  end if;

  select nullif(lower(btrim(p.city)), ''), nullif(lower(btrim(p.company)), ''), p.skills
    into v_city, v_company, v_skills
  from public.profiles p where p.id = v_me;
  v_pat := '%' || replace(replace(replace(lower(coalesce(v_q, '')), '\', '\\'), '%', '\%'), '_', '\_') || '%';

  return coalesce((
    select jsonb_agg(x.card order by x.score desc, x.full_name, x.id)
    from (
      select p.id, p.full_name, s.score,
             jsonb_build_object(
               'id', p.id, 'username', p.username, 'full_name', p.full_name,
               'headline', p.headline, 'company', p.company,
               'city', case when p.hide_city then null else p.city end,
               'avatar_url', p.avatar_url, 'skills', p.skills,
               'profile_visibility', p.profile_visibility,
               'connection_state', public.connection_state(p.id)) as card
      from public.profiles p
      cross join lateral (
        select case when v_mode <> 'suggested' then 0 else
          3 * (select count(*) from public.memberships a
               join public.memberships b on b.community_id = a.community_id
               join public.communities c on c.id = a.community_id and c.deleted_at is null
               where a.user_id = v_me and a.status = 'joined'
                 and b.user_id = p.id and b.status = 'joined')
          + case when v_city is not null and not p.hide_city
                      and lower(btrim(p.city)) = v_city then 2 else 0 end
          + case when v_company is not null and lower(btrim(p.company)) = v_company then 2 else 0 end
          + (select count(*) from jsonb_array_elements_text(p.skills) sk
             where lower(sk) in (select lower(m) from jsonb_array_elements_text(coalesce(v_skills, '[]'::jsonb)) m))
        end::integer as score
      ) s
      where p.id <> v_me
        and not public.blocked_between(v_me, p.id)
        and (v_mode = 'connections' or p.appear_in_search)
        -- mode
        and case v_mode
              when 'connections' then p.id in (select public._connected_ids(v_me))
              when 'suggested' then
                not exists (select 1 from public.connections cn
                            where cn.pair_key = public._pair_key(v_me, p.id)
                              and cn.state in ('pending', 'connected'))
                and public._shares_live_community(v_me, p.id, p_community_id)
              when 'invitable' then
                not exists (select 1 from public.memberships m
                            where m.community_id = p_community_id and m.user_id = p.id
                              and m.status = 'joined')
              else true
            end
        -- community scope (members, connections)
        and (p_community_id is null or v_mode in ('invitable', 'suggested')
             or public._joined_member(p_community_id, p.id))
        -- search text
        and (v_q is null
             or lower(p.full_name) like v_pat
             or lower(coalesce(p.username, '')) like v_pat
             or lower(coalesce(p.headline, '')) like v_pat
             or lower(coalesce(p.company, '')) like v_pat
             or exists (select 1 from jsonb_array_elements_text(p.skills) sk where lower(sk) like v_pat))
        -- profile-based filters
        and case v_filter
              when 'everyone' then true
              when 'same_company' then v_company is not null and lower(btrim(p.company)) = v_company
              else v_city is not null and not p.hide_city and lower(btrim(p.city)) = v_city
            end
      order by s.score desc, p.full_name, p.id
      limit v_limit
    ) x), '[]'::jsonb);
end;
$$;

-- Member profile as the caller may see it. PT404 unknown user, a block
-- either way, or no shared live community (the caller's own profile is
-- always visible). A connections-only profile viewed by a non-connection
-- returns the card only (`limited` true). `city` is omitted when hidden.
-- Adds shared_communities (only communities both are joined to),
-- mutual_connections {count, items[≤6 cards]}, connection_state,
-- connection_id, primary_action (message|connect|pending|respond, null for
-- self), can_message, and follow {followers, following, is_following,
-- can_follow}.
create or replace function public.member_profile(p_user_id uuid)
returns jsonb
language plpgsql stable security definer set search_path = ''
as $$
declare
  v_me uuid := auth.uid();
  v_self boolean;
  p public.profiles;
  v_state text;
  v_conn_id uuid;
  v_limited boolean;
  v_out jsonb;
  v_shared jsonb;
  v_mutual_count integer;
  v_mutual jsonb;
  v_followers integer;
  v_following integer;
begin
  if v_me is null then
    raise exception 'Sign in required.' using errcode = 'PT401';
  end if;
  v_self := p_user_id = v_me;
  select * into p from public.profiles pr where pr.id = p_user_id;
  if not found or (not v_self and (public.blocked_between(v_me, p_user_id)
                                   or not public._shares_live_community(v_me, p_user_id))) then
    raise exception 'Profile not found.' using errcode = 'PT404';
  end if;

  v_state := case when v_self then 'self' else public.connection_state(p_user_id) end;
  if v_state in ('sent', 'incoming', 'connected') then
    select c.id into v_conn_id from public.connections c
    where c.pair_key = public._pair_key(v_me, p_user_id);
  end if;
  v_limited := p.profile_visibility = 'connections' and v_state not in ('connected', 'self');

  v_out := jsonb_build_object(
    'id', p.id, 'username', p.username, 'full_name', p.full_name,
    'headline', p.headline, 'avatar_url', p.avatar_url,
    'profile_visibility', p.profile_visibility, 'limited', v_limited,
    'created_at', p.created_at);
  if not v_limited then
    v_out := v_out || jsonb_build_object(
      'company', p.company, 'bio', p.bio, 'skills', p.skills, 'interests', p.interests,
      'onboarding_complete', p.onboarding_complete, 'updated_at', p.updated_at);
    if v_self or not p.hide_city then
      v_out := v_out || jsonb_build_object('city', p.city);
    end if;
  end if;

  select coalesce(jsonb_agg(jsonb_build_object(
           'id', c.id, 'handle', c.handle, 'name', c.name, 'emoji', c.emoji,
           'logo_url', c.logo_url, 'cover_gradient', c.cover_gradient,
           'member_count', c.member_count) order by c.name, c.id), '[]'::jsonb)
    into v_shared
  from public.communities c
  where c.deleted_at is null
    and exists (select 1 from public.memberships a
                where a.community_id = c.id and a.user_id = v_me and a.status = 'joined')
    and exists (select 1 from public.memberships b
                where b.community_id = c.id and b.user_id = p_user_id and b.status = 'joined');

  with m as (
    select id from public._connected_ids(v_me) id
    intersect
    select id from public._connected_ids(p_user_id) id
  ), visible as (
    select m.id, pr.full_name from m join public.profiles pr on pr.id = m.id
    where not v_self and m.id not in (v_me, p_user_id)
      and not public.blocked_between(v_me, m.id)
  )
  select (select count(*)::integer from visible),
         coalesce((select jsonb_agg(public._profile_card(t.id) order by t.full_name, t.id)
                   from (select * from visible order by full_name, id limit 6) t), '[]'::jsonb)
    into v_mutual_count, v_mutual;

  select f.followers, f.following into v_followers, v_following from public.follow_counts(p_user_id) f;

  return v_out || jsonb_build_object(
    'shared_communities', v_shared,
    'mutual_connections', jsonb_build_object('count', v_mutual_count, 'items', v_mutual),
    'connection_state', v_state,
    'connection_id', v_conn_id,
    'primary_action', case v_state
                        when 'connected' then 'message'
                        when 'sent' then 'pending'
                        when 'incoming' then 'respond'
                        when 'idle' then 'connect'
                      end,
    'can_message', v_state = 'connected',
    'follow', jsonb_build_object(
      'followers', v_followers, 'following', v_following,
      'is_following', exists (select 1 from public.follows f
                              where f.follower_id = v_me and f.followee_id = p_user_id),
      'can_follow', not v_self and p.profile_visibility = 'public'));
end;
$$;

revoke all on function
  public._shares_live_community(uuid, uuid, uuid), public._connected_ids(uuid),
  public.search_people(text, uuid, text, text, integer), public.member_profile(uuid)
  from public, anon, authenticated;

grant execute on function
  public._shares_live_community(uuid, uuid, uuid), public._connected_ids(uuid)
  to service_role;
grant execute on function
  public.search_people(text, uuid, text, text, integer), public.member_profile(uuid)
  to authenticated, service_role;
