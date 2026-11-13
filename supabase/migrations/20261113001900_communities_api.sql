-- Community create, update, overview, listing, deletion and restore
-- (createCommunity, updateCommunity, getCommunity, listCommunities,
-- deleteCommunity, restoreCommunity).

-- ---------------------------------------------------------------------------
-- Catalogues
-- ---------------------------------------------------------------------------

-- The 37 City/Area choices for place-based communities.
create or replace function public.community_cities()
returns text[]
language sql immutable set search_path = ''
as $$
  select array[
    'Agra', 'Ahmedabad', 'Amritsar', 'Bengaluru', 'Bhopal', 'Bhubaneswar',
    'Chandigarh', 'Chennai', 'Coimbatore', 'Dehradun', 'Delhi', 'Faridabad',
    'Ghaziabad', 'Goa', 'Gurugram', 'Guwahati', 'Hyderabad', 'Indore',
    'Jaipur', 'Jodhpur', 'Kanpur', 'Kochi', 'Kolkata', 'Lucknow',
    'Ludhiana', 'Madurai', 'Mumbai', 'Mysuru', 'Nagpur', 'Nashik',
    'Noida', 'Patna', 'Pune', 'Raipur', 'Surat', 'Vadodara', 'Visakhapatnam'
  ]::text[]
$$;

-- Category → interest rules. mode: fixed (listed chips only, no custom input),
-- limited (listed chips only), open (listed chips plus custom free text).
-- primary is auto-applied when the category is chosen.
create or replace function public.community_categories()
returns jsonb
language sql immutable set search_path = ''
as $$
  select '[
    {"category": "Education", "mode": "limited", "primary": "Education",
     "options": ["Education", "School", "College", "Coaching", "Alumni", "Tutoring", "Language"]},
    {"category": "Club", "mode": "open", "primary": "Club",
     "options": ["Club", "Book Club", "Photography", "Music", "Travel", "Food", "Tech"]},
    {"category": "Association", "mode": "limited", "primary": "Association",
     "options": ["Association", "Residents", "Professional", "Alumni", "Trade", "Cultural", "Samaj"]},
    {"category": "Event Organizer", "mode": "open", "primary": "Events",
     "options": ["Events", "Meetups", "Workshops", "Conferences", "Concerts", "Festivals"]},
    {"category": "Sports & Play", "mode": "limited", "primary": "Sports",
     "options": ["Sports", "Cricket", "Football", "Badminton", "Tennis", "Running", "Cycling", "Chess", "Basketball"]},
    {"category": "Fitness & Gym", "mode": "fixed", "primary": "Fitness",
     "options": ["Fitness", "Gym", "Yoga", "CrossFit", "Zumba", "Pilates"]},
    {"category": "Other", "mode": "open", "primary": null, "options": []}
  ]'::jsonb
$$;

-- Trimmed, non-blank, case-insensitively deduplicated strings of a jsonb
-- array, in first-seen order. PT400 when an element is not a string.
create or replace function public._clean_string_array(p jsonb, p_what text)
returns jsonb
language plpgsql immutable set search_path = ''
as $$
declare
  v_out jsonb := '[]'::jsonb;
  v_seen text[] := '{}';
  v_el jsonb;
  v_s text;
begin
  if p is null or jsonb_typeof(p) = 'null' then
    return '[]'::jsonb;
  end if;
  if jsonb_typeof(p) <> 'array' then
    raise exception '% must be a list of text.', p_what using errcode = 'PT400';
  end if;
  for v_el in select value from jsonb_array_elements(p) loop
    if jsonb_typeof(v_el) <> 'string' then
      raise exception '% must be a list of text.', p_what using errcode = 'PT400';
    end if;
    v_s := btrim(v_el #>> '{}');
    if v_s <> '' and not (lower(v_s) = any (v_seen)) then
      v_seen := v_seen || lower(v_s);
      v_out := v_out || to_jsonb(v_s);
    end if;
  end loop;
  return v_out;
end
$$;

-- Applies the category rules: primary first, unlisted interest → PT400 for
-- fixed/limited categories; listed chips take the catalogue's spelling.
create or replace function public._category_interests(p_category text, p_interests jsonb)
returns jsonb
language plpgsql immutable set search_path = ''
as $$
declare
  v_cat jsonb;
  v_out jsonb := '[]'::jsonb;
  v_s text;
  v_listed text;
begin
  if p_category is null then
    return public._clean_string_array(p_interests, 'Interests');
  end if;
  select c into v_cat from jsonb_array_elements(public.community_categories()) c
  where c->>'category' = p_category;
  if v_cat is null then
    raise exception 'Choose a category from the list.' using errcode = 'PT400';
  end if;
  if v_cat->>'primary' is not null then
    v_out := jsonb_build_array(v_cat->>'primary');
  end if;
  for v_s in select value #>> '{}' from jsonb_array_elements(public._clean_string_array(p_interests, 'Interests')) loop
    select o #>> '{}' into v_listed from jsonb_array_elements(v_cat->'options') o
    where lower(o #>> '{}') = lower(v_s);
    if v_listed is null and v_cat->>'mode' <> 'open' then
      raise exception '"%" is not an interest for %.', v_s, p_category using errcode = 'PT400';
    end if;
    v_out := v_out || to_jsonb(coalesce(v_listed, v_s));
  end loop;
  return public._clean_string_array(v_out, 'Interests');
end
$$;

-- ---------------------------------------------------------------------------
-- createCommunity: validated, idempotent on creation_key, concurrency-safe.
-- ---------------------------------------------------------------------------

create or replace function public.create_community(
  p_name text,
  p_location_type text,
  p_creation_key uuid,
  p_city text default null,
  p_visibility text default 'private'
)
returns public.communities
language plpgsql security definer set search_path = ''
as $$
declare
  v_uid uuid := auth.uid();
  v_row public.communities;
  v_base text;
  v_handle text;
  v_city text;
  v_n integer := 1;
begin
  if v_uid is null then
    raise exception 'Sign in to create a community.' using errcode = 'PT401';
  end if;
  if p_creation_key is null then
    raise exception 'creation_key is required.' using errcode = 'PT400';
  end if;

  -- A double-tap sends the same key twice: the second call waits here and
  -- then returns the first call's community.
  perform pg_advisory_xact_lock(hashtextextended('community-key:' || p_creation_key::text, 0));

  select * into v_row from public.communities where creation_key = p_creation_key;
  if found then
    if v_row.created_by <> v_uid then
      raise exception 'creation_key already used' using errcode = 'PT409';
    end if;
    return v_row;
  end if;

  if btrim(coalesce(p_name, '')) = '' then
    raise exception 'Enter a community name.' using errcode = 'PT400';
  end if;
  if length(btrim(p_name)) > 80 then
    raise exception 'Community name must be 80 characters or fewer.' using errcode = 'PT400';
  end if;
  if p_location_type is null or p_location_type not in ('place', 'online') then
    raise exception 'Choose Place-based or Online only.' using errcode = 'PT400';
  end if;
  if p_location_type = 'place' then
    select c into v_city from unnest(public.community_cities()) c
    where lower(c) = lower(btrim(coalesce(p_city, '')));
    if v_city is null then
      raise exception 'Enter a city or area.' using errcode = 'PT400';
    end if;
  end if;
  if coalesce(p_visibility, 'private') not in ('private', 'public') then
    raise exception 'Visibility must be private or public.' using errcode = 'PT400';
  end if;

  -- Handle: lowercase alphanumerics and hyphens; room for a "-NN" suffix
  -- inside 40 characters.
  v_base := btrim(regexp_replace(lower(btrim(p_name)), '[^a-z0-9]+', '-', 'g'), '-');
  v_base := btrim(left(v_base, 34), '-');
  if v_base = '' then
    v_base := 'community';
  end if;
  perform pg_advisory_xact_lock(hashtextextended('community-handle:' || v_base, 0));
  v_handle := v_base;
  while exists (select 1 from public.communities where handle = v_handle) loop
    v_n := v_n + 1;
    v_handle := v_base || '-' || v_n;
  end loop;

  insert into public.communities (handle, name, location_type, city, visibility, created_by, creation_key)
  values (v_handle, btrim(p_name), p_location_type, v_city,
          coalesce(p_visibility, 'private'), v_uid, p_creation_key)
  returning * into v_row;

  insert into public.memberships (community_id, user_id, role, status, via)
  values (v_row.id, v_uid, 'owner', 'joined', 'direct');

  select * into v_row from public.communities where id = v_row.id;
  return v_row;
end
$$;

-- ---------------------------------------------------------------------------
-- Shared JSON shapes
-- ---------------------------------------------------------------------------

create or replace function public._community_questions_json(p_community_id uuid)
returns jsonb
language sql stable security definer set search_path = ''
as $$
  select coalesce(jsonb_agg(to_jsonb(q) - 'community_id' order by q.position, q.id), '[]'::jsonb)
  from public.community_questions q where q.community_id = p_community_id
$$;

-- Card decoration for the caller: membership state, role and badge.
create or replace function public._community_card(p_c public.communities)
returns jsonb
language sql stable security definer set search_path = ''
as $$
  select to_jsonb(p_c) || jsonb_build_object(
    'membership_state', coalesce(m.status, 'none'),
    'role', case when m.status = 'joined' then m.role end,
    'role_badge', case when m.status is distinct from 'joined' then null
                       when m.role in ('owner', 'admin') then 'You manage'
                       else 'Member of' end)
  from (select 1) one
  left join public.memberships m on m.community_id = p_c.id and m.user_id = auth.uid()
$$;

-- ---------------------------------------------------------------------------
-- updateCommunity
-- ---------------------------------------------------------------------------

-- p_patch keys (all optional):
--   profile (owner or admin): name, category, interests, emoji, cover_gradient,
--     logo_url, cover_url, rules
--   settings (owner only): visibility, invite_permission, join_approval,
--     open_to_collaborate, location_type, city, questions
-- Unknown and read-only keys are ignored. questions replaces the list; an
-- item with the id of an existing question updates it (keeps its answers).
create or replace function public.update_community(p_community_id uuid, p_patch jsonb)
returns jsonb
language plpgsql security definer set search_path = ''
as $$
declare
  v_c public.communities;
  v_role text;
  v_settings constant text[] := array['visibility', 'invite_permission', 'join_approval',
    'open_to_collaborate', 'location_type', 'city', 'questions'];
  v_rules jsonb;
  v_q jsonb;
  v_kind text;
  v_opts jsonb;
  v_keep uuid[] := '{}';
  v_qid uuid;
  v_pos integer := 0;
  v_city text;
  v_loc text;
begin
  if p_patch is null or jsonb_typeof(p_patch) <> 'object' then
    raise exception 'Send the fields to change as a JSON object.' using errcode = 'PT400';
  end if;
  select * into v_c from public.communities where id = p_community_id and deleted_at is null for update;
  if not found then
    raise exception 'Community not found' using errcode = 'PT404';
  end if;
  v_role := public.community_role(p_community_id);
  if v_role is null or v_role not in ('owner', 'admin') then
    raise exception 'Only the owner or an admin can edit this community.' using errcode = 'PT403';
  end if;
  if v_role <> 'owner' and exists (select 1 from jsonb_object_keys(p_patch) k where k = any (v_settings)) then
    raise exception 'Only the owner can change privacy and admission settings.' using errcode = 'PT403';
  end if;

  -- Profile
  if p_patch ? 'name' then
    if btrim(coalesce(p_patch->>'name', '')) = '' then
      raise exception 'Enter a community name.' using errcode = 'PT400';
    end if;
    v_c.name := btrim(p_patch->>'name');
  end if;
  if p_patch ? 'category' then
    v_c.category := nullif(btrim(coalesce(p_patch->>'category', '')), '');
    if not (p_patch ? 'interests') then
      -- A new category replaces the old chips with its primary interest.
      v_c.interests := public._category_interests(v_c.category, '[]'::jsonb);
    end if;
  end if;
  if p_patch ? 'interests' then
    v_c.interests := public._category_interests(v_c.category, p_patch->'interests');
  end if;
  if p_patch ? 'emoji' then
    v_c.emoji := coalesce(nullif(btrim(coalesce(p_patch->>'emoji', '')), ''), '🌱');
  end if;
  if p_patch ? 'cover_gradient' then
    v_c.cover_gradient := coalesce(nullif(btrim(coalesce(p_patch->>'cover_gradient', '')), ''), 'sunrise');
  end if;
  if p_patch ? 'logo_url' then
    v_c.logo_url := nullif(btrim(coalesce(p_patch->>'logo_url', '')), '');
  end if;
  if p_patch ? 'cover_url' then
    v_c.cover_url := nullif(btrim(coalesce(p_patch->>'cover_url', '')), '');
  end if;
  if p_patch ? 'rules' then
    v_rules := public._clean_string_array(p_patch->'rules', 'Rules');
    if jsonb_array_length(v_rules) = 0 then
      raise exception 'Add at least one rule.' using errcode = 'PT400';
    end if;
    if exists (select 1 from jsonb_array_elements_text(v_rules) r where length(r) > 500) then
      raise exception 'Each rule must be 500 characters or fewer.' using errcode = 'PT400';
    end if;
    v_c.rules := v_rules;
  end if;

  -- Settings
  if p_patch ? 'visibility' then
    if coalesce(p_patch->>'visibility', '') not in ('private', 'public') then
      raise exception 'Visibility must be private or public.' using errcode = 'PT400';
    end if;
    v_c.visibility := p_patch->>'visibility';
  end if;
  if p_patch ? 'invite_permission' then
    if coalesce(p_patch->>'invite_permission', '') not in ('admins', 'admins_moderators', 'all_members') then
      raise exception 'Invite permission must be admins, admins_moderators or all_members.' using errcode = 'PT400';
    end if;
    v_c.invite_permission := p_patch->>'invite_permission';
  end if;
  if p_patch ? 'join_approval' then
    if jsonb_typeof(p_patch->'join_approval') <> 'boolean' then
      raise exception 'join_approval must be true or false.' using errcode = 'PT400';
    end if;
    v_c.join_approval := (p_patch->>'join_approval')::boolean;
  end if;
  if p_patch ? 'open_to_collaborate' then
    if jsonb_typeof(p_patch->'open_to_collaborate') <> 'boolean' then
      raise exception 'open_to_collaborate must be true or false.' using errcode = 'PT400';
    end if;
    v_c.open_to_collaborate := (p_patch->>'open_to_collaborate')::boolean;
  end if;
  if p_patch ? 'location_type' or p_patch ? 'city' then
    v_loc := coalesce(p_patch->>'location_type', v_c.location_type);
    if v_loc not in ('place', 'online') then
      raise exception 'Choose Place-based or Online only.' using errcode = 'PT400';
    end if;
    v_city := null;
    if v_loc = 'place' then
      select c into v_city from unnest(public.community_cities()) c
      where lower(c) = lower(btrim(coalesce(case when p_patch ? 'city' then p_patch->>'city' else v_c.city end, '')));
      if v_city is null then
        raise exception 'Enter a city or area.' using errcode = 'PT400';
      end if;
    end if;
    v_c.location_type := v_loc;
    v_c.city := v_city;
  end if;

  update public.communities set
    name = v_c.name, category = v_c.category, interests = v_c.interests,
    emoji = v_c.emoji, cover_gradient = v_c.cover_gradient, logo_url = v_c.logo_url,
    cover_url = v_c.cover_url, rules = v_c.rules, visibility = v_c.visibility,
    invite_permission = v_c.invite_permission, join_approval = v_c.join_approval,
    open_to_collaborate = v_c.open_to_collaborate, location_type = v_c.location_type,
    city = v_c.city
  where id = p_community_id
  returning * into v_c;

  if p_patch ? 'questions' then
    if jsonb_typeof(p_patch->'questions') <> 'array' then
      raise exception 'questions must be a list.' using errcode = 'PT400';
    end if;
    if jsonb_array_length(p_patch->'questions') > 20 then
      raise exception 'Add at most 20 questions.' using errcode = 'PT400';
    end if;
    for v_q in select value from jsonb_array_elements(p_patch->'questions') loop
      if jsonb_typeof(v_q) <> 'object' or btrim(coalesce(v_q->>'label', '')) = '' then
        raise exception 'Each question needs a label.' using errcode = 'PT400';
      end if;
      v_kind := v_q->>'kind';
      if v_kind = 'file' then
        raise exception 'File questions are not supported.' using errcode = 'PT400';
      end if;
      if v_kind is null or v_kind not in ('short_text', 'long_text', 'single_choice', 'multi_choice', 'number', 'date', 'url') then
        raise exception 'Question type must be short_text, long_text, single_choice, multi_choice, number, date or url.' using errcode = 'PT400';
      end if;
      v_opts := null;
      if v_kind in ('single_choice', 'multi_choice') then
        v_opts := public._clean_string_array(v_q->'options', 'Options');
        if jsonb_array_length(v_opts) = 0 then
          raise exception 'Add at least one option to a choice question.' using errcode = 'PT400';
        end if;
      end if;
      v_qid := null;
      if v_q ? 'id' and v_q->>'id' ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then
        update public.community_questions set
          label = btrim(v_q->>'label'), kind = v_kind,
          why_we_ask = nullif(btrim(coalesce(v_q->>'why_we_ask', '')), ''),
          required = coalesce((v_q->>'required')::boolean, false),
          options = v_opts, position = v_pos
        where id = (v_q->>'id')::uuid and community_id = p_community_id
        returning id into v_qid;
      end if;
      if v_qid is null then
        insert into public.community_questions (community_id, label, kind, why_we_ask, required, options, position)
        values (p_community_id, btrim(v_q->>'label'), v_kind,
                nullif(btrim(coalesce(v_q->>'why_we_ask', '')), ''),
                coalesce((v_q->>'required')::boolean, false), v_opts, v_pos)
        returning id into v_qid;
      end if;
      v_keep := v_keep || v_qid;
      v_pos := v_pos + 1;
    end loop;
    delete from public.community_questions
    where community_id = p_community_id and not (id = any (v_keep));
  end if;

  return to_jsonb(v_c) || jsonb_build_object('questions', public._community_questions_json(p_community_id));
end
$$;

-- ---------------------------------------------------------------------------
-- getCommunity: overview
-- ---------------------------------------------------------------------------

-- Overview by membership state. Non-members get the public profile, rules,
-- join questions and "What's inside" counts only (no post content).
-- Private communities need a membership or a usable invite code (PT403
-- 'Invitation required'). The owner also gets the setup checklist. The
-- first joined view stamps welcome_seen_at (show_welcome true once).
create or replace function public.community_overview(p_community_id uuid, p_invite_code text default null)
returns jsonb
language plpgsql security definer set search_path = ''
as $$
declare
  v_uid uuid := auth.uid();
  v_c public.communities;
  v_m public.memberships;
  v_state text := 'none';
  v_invite boolean := false;
  v_welcome boolean := false;
  v_inside jsonb;
  v_checklist jsonb;
  v_steps jsonb;
  v_done integer;
begin
  select * into v_c from public.communities where id = p_community_id and deleted_at is null;
  if not found then
    raise exception 'Community not found' using errcode = 'PT404';
  end if;
  select * into v_m from public.memberships where community_id = p_community_id and user_id = v_uid;
  if found then
    v_state := v_m.status;
  end if;
  if v_state = 'none' and nullif(btrim(coalesce(p_invite_code, '')), '') is not null then
    v_invite := exists (
      select 1 from public.invitations i
      where i.community_id = p_community_id and i.code = btrim(p_invite_code)
        and i.revoked_at is null and i.expires_at > now() and i.use_count < i.max_uses);
  end if;
  if v_c.visibility = 'private' and v_state = 'none' and not v_invite then
    raise exception 'Invitation required' using errcode = 'PT403';
  end if;

  if v_state = 'joined' and v_m.welcome_seen_at is null then
    update public.memberships set welcome_seen_at = now() where id = v_m.id;
    v_welcome := v_m.via <> 'direct';
  end if;

  v_inside := jsonb_build_object(
    'posts_per_week', (select count(*) from public.post_targets t join public.posts p on p.id = t.post_id
                       where t.community_id = p_community_id and p.deleted_at is null
                         and t.created_at > now() - interval '7 days'),
    'upcoming_events', (select count(*) from public.events e
                        where e.community_id = p_community_id and e.deleted_at is null and e.ends_at > now()),
    'active_chats', (select count(*) from public.threads th
                     where th.community_id = p_community_id and th.last_message_at > now() - interval '7 days'),
    'groups', (select count(*) from public.threads th where th.community_id = p_community_id and th.kind = 'group'),
    'intros', (select count(*) from public.memberships mm
               where mm.community_id = p_community_id and mm.status = 'joined'
                 and mm.created_at > now() - interval '30 days'));

  if v_state = 'joined' and v_m.role = 'owner' then
    v_steps := jsonb_build_array(
      jsonb_build_object('key', 'invite_members', 'label', 'Invite first members',
        'done', v_c.member_count > 1 or exists (select 1 from public.invitations i where i.community_id = p_community_id)),
      jsonb_build_object('key', 'first_post', 'label', 'Write first post',
        'done', exists (select 1 from public.post_targets t join public.posts p on p.id = t.post_id
                        where t.community_id = p_community_id and p.deleted_at is null)),
      jsonb_build_object('key', 'first_group', 'label', 'Create first group',
        'done', exists (select 1 from public.threads th where th.community_id = p_community_id and th.kind = 'group')),
      jsonb_build_object('key', 'schedule_event', 'label', 'Schedule an event',
        'done', exists (select 1 from public.events e where e.community_id = p_community_id and e.deleted_at is null)),
      jsonb_build_object('key', 'set_rules', 'label', 'Set rules', 'done', v_c.rules is not null));
    select count(*) into v_done from jsonb_array_elements(v_steps) s where (s->>'done')::boolean;
    v_checklist := jsonb_build_object('items', v_steps, 'percent_complete', v_done * 20);
  end if;

  return to_jsonb(v_c) || jsonb_build_object(
    'membership_state', v_state,
    'role', case when v_state = 'joined' then v_m.role end,
    'role_badge', case when v_state <> 'joined' then null
                       when v_m.role in ('owner', 'admin') then 'You manage' else 'Member of' end,
    'content_access', case when v_state = 'joined' then 'member' else 'members_only' end,
    'content_message', case when v_state = 'joined' then null else 'Members only' end,
    'join_action', case when v_state = 'joined' then null
                        when v_state = 'pending' then 'pending'
                        when v_invite then 'accept_invite'
                        when v_c.visibility = 'public' then 'join' end,
    'whats_inside', v_inside,
    'questions', case when v_state = 'joined' and v_m.role not in ('owner', 'admin') then null
                      else public._community_questions_json(p_community_id) end,
    'show_welcome', v_welcome,
    'checklist', v_checklist);
end
$$;

-- ---------------------------------------------------------------------------
-- listCommunities
-- ---------------------------------------------------------------------------

-- view: mine (joined, live; pending excluded) | trending | popular |
-- recommended (excludes communities the caller has any membership in) |
-- search. Every view except mine lists public communities only. q filters
-- by name, category or interest (case-insensitive substring).
create or replace function public.list_communities(p_view text, p_q text default null, p_limit integer default 50)
returns jsonb
language plpgsql stable security definer set search_path = ''
as $$
declare
  v_uid uuid := auth.uid();
  v_q text := nullif(btrim(coalesce(p_q, '')), '');
  v_pat text;
  v_lim integer := least(greatest(coalesce(p_limit, 50), 1), 100);
  v_out jsonb;
begin
  if p_view is null or p_view not in ('mine', 'trending', 'popular', 'recommended', 'search') then
    raise exception 'view must be mine, trending, popular, recommended or search.' using errcode = 'PT400';
  end if;
  v_pat := '%' || replace(replace(replace(lower(coalesce(v_q, '')), '\', '\\'), '%', '\%'), '_', '\_') || '%';

  with base as (
    select c.*,
      (select count(*) from public.memberships mm where mm.community_id = c.id and mm.status = 'joined'
         and mm.created_at > now() - interval '7 days')
      + (select count(*) from public.post_targets t where t.community_id = c.id
         and t.created_at > now() - interval '7 days') as activity,
      (select count(*) from jsonb_array_elements_text(c.interests) i
         join public.profiles p on p.id = v_uid
         where exists (select 1 from jsonb_array_elements_text(p.interests) pi where lower(pi) = lower(i)))
      + (select case when c.city is not null and lower(c.city) = lower(p.city) then 2 else 0 end
         from public.profiles p where p.id = v_uid) as affinity
    from public.communities c
    where c.deleted_at is null
      and (v_q is null
           or lower(c.name) like v_pat
           or lower(coalesce(c.category, '')) like v_pat
           or exists (select 1 from jsonb_array_elements_text(c.interests) i where lower(i) like v_pat))
      and case p_view
            when 'mine' then exists (select 1 from public.memberships m
                                     where m.community_id = c.id and m.user_id = v_uid and m.status = 'joined')
            when 'recommended' then c.visibility = 'public'
                                and not exists (select 1 from public.memberships m
                                                where m.community_id = c.id and m.user_id = v_uid)
            else c.visibility = 'public'
          end
  ), ranked as (
    select b.*, row_number() over (order by
        case when p_view = 'trending' then b.activity end desc nulls last,
        case when p_view = 'recommended' then coalesce(b.affinity, 0) end desc nulls last,
        case when p_view = 'mine' then b.name end asc,
        b.member_count desc, b.created_at desc, b.id) as rn
    from base b
  )
  select coalesce(jsonb_agg(public._community_card(
           (select c2 from public.communities c2 where c2.id = r.id)) order by r.rn), '[]'::jsonb)
  into v_out
  from ranked r where r.rn <= v_lim;
  return v_out;
end
$$;

-- ---------------------------------------------------------------------------
-- deleteCommunity / restoreCommunity
-- ---------------------------------------------------------------------------

-- Soft delete by the owner after typing the community name. Idempotent:
-- a second call keeps the first deleted_at.
create or replace function public.delete_community(p_community_id uuid, p_confirm_name text)
returns jsonb
language plpgsql security definer set search_path = ''
as $$
declare
  v_c public.communities;
begin
  select * into v_c from public.communities where id = p_community_id for update;
  if not found then
    raise exception 'Community not found' using errcode = 'PT404';
  end if;
  if not public.has_community_role(p_community_id, array['owner']) then
    raise exception 'Only the owner can delete this community.' using errcode = 'PT403';
  end if;
  if p_confirm_name is null or btrim(p_confirm_name) <> v_c.name then
    raise exception 'Type the community name exactly to confirm.' using errcode = 'PT400';
  end if;
  update public.communities set deleted_at = coalesce(deleted_at, now())
  where id = p_community_id returning * into v_c;
  return jsonb_build_object('id', v_c.id, 'deleted_at', v_c.deleted_at,
                            'purge_at', v_c.deleted_at + interval '14 days');
end
$$;

-- Restores a soft-deleted community within 14 days (PT410 after). Rows that
-- belong to it were never touched, so it returns intact.
create or replace function public.restore_community(p_community_id uuid)
returns public.communities
language plpgsql security definer set search_path = ''
as $$
declare
  v_c public.communities;
begin
  select * into v_c from public.communities where id = p_community_id for update;
  if not found then
    raise exception 'Community not found' using errcode = 'PT404';
  end if;
  if not public.has_community_role(p_community_id, array['owner']) then
    raise exception 'Only the owner can restore this community.' using errcode = 'PT403';
  end if;
  if v_c.deleted_at is not null and v_c.deleted_at <= now() - interval '14 days' then
    raise exception 'The 14-day restore period is over.' using errcode = 'PT410';
  end if;
  update public.communities set deleted_at = null where id = p_community_id returning * into v_c;
  return v_c;
end
$$;

-- Scheduler: communities past the 14-day grace period (service_role only).
create or replace function public.communities_due_for_purge(p_limit integer default 100)
returns setof uuid
language sql stable security definer set search_path = ''
as $$
  select id from public.communities
  where deleted_at is not null and deleted_at <= now() - interval '14 days'
  order by deleted_at limit greatest(coalesce(p_limit, 100), 1)
$$;

revoke all on function
  public.community_cities(), public.community_categories(),
  public._clean_string_array(jsonb, text), public._category_interests(text, jsonb),
  public.create_community(text, text, uuid, text, text),
  public._community_questions_json(uuid), public._community_card(public.communities),
  public.update_community(uuid, jsonb), public.community_overview(uuid, text),
  public.list_communities(text, text, integer), public.delete_community(uuid, text),
  public.restore_community(uuid), public.communities_due_for_purge(integer)
  from public, anon, authenticated;

grant execute on function
  public.community_cities(), public.community_categories(),
  public.create_community(text, text, uuid, text, text),
  public.update_community(uuid, jsonb), public.community_overview(uuid, text),
  public.list_communities(text, text, integer), public.delete_community(uuid, text),
  public.restore_community(uuid)
  to authenticated;
grant execute on function public.community_cities(), public.community_categories() to anon;
grant execute on function public.communities_due_for_purge(integer) to service_role;

-- Community row JSON for create/restore responses; questions are included
-- for the community's owner and admins only.
create or replace function public._community_json(p_c public.communities)
returns jsonb
language sql stable security definer set search_path = ''
as $$
  select to_jsonb(p_c) || jsonb_build_object('questions',
    case when public.has_community_role(p_c.id, array['owner', 'admin'])
         then public._community_questions_json(p_c.id) else '[]'::jsonb end)
$$;
revoke all on function public._community_json(public.communities) from public, anon;
grant execute on function public._community_json(public.communities) to authenticated;
