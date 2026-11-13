-- Identity, community and membership schema with RLS.
-- Tables: profiles, communities, memberships, community_questions,
-- join_answers, invitations. Helper functions, triggers and the
-- SECURITY DEFINER RPCs that perform role-sensitive multi-row writes.
--
-- Error convention for RPCs: SQLSTATE 'PTnnn' (PostgREST turns it into HTTP
-- status nnn), message is the user-facing text.

create extension if not exists pgcrypto with schema extensions;

-- ---------------------------------------------------------------------------
-- Tables
-- ---------------------------------------------------------------------------

create table public.profiles (
    id                     uuid primary key references auth.users (id) on delete cascade,
    username               varchar(30)
        constraint profiles_username_format check (username ~ '^[a-z0-9_.]{3,30}$'),
    full_name              varchar(80) not null
        constraint profiles_full_name_length check (char_length(btrim(full_name)) between 2 and 80),
    headline               varchar(120),
    company                varchar(100),
    city                   varchar(60),
    bio                    varchar(160),
    avatar_url             text,
    skills                 jsonb not null default '[]'::jsonb
        constraint profiles_skills_array check (jsonb_typeof(skills) = 'array'),
    interests              jsonb not null default '[]'::jsonb
        constraint profiles_interests_array check (jsonb_typeof(interests) = 'array'),
    onboarding_complete    boolean not null default false,
    profile_visibility     text not null default 'public'
        constraint profiles_visibility_check check (profile_visibility in ('public', 'connections')),
    appear_in_search       boolean not null default true,
    hide_city              boolean not null default false,
    activity_status        boolean not null default true,
    phone                  varchar(15)
        constraint profiles_phone_format check (phone ~ '^\+[1-9][0-9]{6,14}$'),
    two_factor_enabled     boolean not null default false,
    login_alerts           boolean not null default true,
    is_operator            boolean not null default false,
    deletion_requested_at  timestamptz,
    deletion_reason        varchar(500),
    created_at             timestamptz not null default now(),
    updated_at             timestamptz not null default now(),
    constraint profiles_two_factor_needs_phone check (not two_factor_enabled or phone is not null)
);

-- Case-insensitive uniqueness; the index decides concurrent-save races.
create unique index profiles_username_key on public.profiles (lower(username));
create unique index profiles_phone_key on public.profiles (phone);

create table public.communities (
    id                   uuid primary key default gen_random_uuid(),
    handle               varchar(40) not null
        constraint communities_handle_format check (handle ~ '^[a-z0-9]+(-[a-z0-9]+)*$'),
    name                 varchar(80) not null
        constraint communities_name_not_blank check (btrim(name) <> ''),
    category             text
        constraint communities_category_check check (category in
            ('Education', 'Club', 'Association', 'Event Organizer', 'Sports & Play', 'Fitness & Gym', 'Other')),
    interests            jsonb not null default '[]'::jsonb
        constraint communities_interests_array check (jsonb_typeof(interests) = 'array'),
    location_type        text not null
        constraint communities_location_type_check check (location_type in ('place', 'online')),
    city                 varchar(60),
    visibility           text not null default 'private'
        constraint communities_visibility_check check (visibility in ('private', 'public')),
    invite_permission    text not null default 'admins'
        constraint communities_invite_permission_check check (invite_permission in ('admins', 'admins_moderators', 'all_members')),
    join_approval        boolean not null default true,
    open_to_collaborate  boolean not null default false,
    emoji                varchar(8) not null default '🌱',
    cover_gradient       varchar(40) not null default 'sunrise',
    logo_url             text,
    cover_url            text,
    rules                jsonb
        constraint communities_rules_array check (rules is null or (jsonb_typeof(rules) = 'array' and jsonb_array_length(rules) >= 1)),
    member_count         integer not null default 0
        constraint communities_member_count_nonnegative check (member_count >= 0),
    created_by           uuid not null references public.profiles (id),
    creation_key         uuid not null,
    deleted_at           timestamptz,
    created_at           timestamptz not null default now(),
    updated_at           timestamptz not null default now(),
    constraint communities_city_iff_place check ((location_type = 'place') = (city is not null))
);

create unique index communities_handle_key on public.communities (handle);
create unique index communities_creation_key_key on public.communities (creation_key);

create table public.invitations (
    id               uuid primary key default gen_random_uuid(),
    community_id     uuid not null references public.communities (id) on delete cascade,
    code             varchar(32) not null default encode(extensions.gen_random_bytes(16), 'hex'),
    channel          text not null
        constraint invitations_channel_check check (channel in ('link', 'whatsapp', 'email', 'in_app')),
    invitee_email    varchar(255),
    invitee_user_id  uuid references public.profiles (id) on delete cascade,
    created_by       uuid not null references public.profiles (id),
    expires_at       timestamptz not null default now() + interval '14 days',
    max_uses         integer not null default 1
        constraint invitations_max_uses_positive check (max_uses >= 1),
    use_count        integer not null default 0,
    revoked_at       timestamptz,
    tracking_state   text not null default 'sent'
        constraint invitations_tracking_state_check check (tracking_state in ('sent', 'opened', 'accepted', 'failed')),
    created_at       timestamptz not null default now(),
    constraint invitations_use_count_range check (use_count between 0 and max_uses),
    constraint invitations_email_channel check ((channel = 'email') = (invitee_email is not null)),
    constraint invitations_in_app_channel check ((channel = 'in_app') = (invitee_user_id is not null))
);

create unique index invitations_code_key on public.invitations (code);
create index invitations_community_id_idx on public.invitations (community_id);

create table public.memberships (
    id               uuid primary key default gen_random_uuid(),
    community_id     uuid not null references public.communities (id) on delete cascade,
    user_id          uuid not null references public.profiles (id) on delete cascade,
    role             text not null default 'member'
        constraint memberships_role_check check (role in ('owner', 'admin', 'moderator', 'member')),
    status           text not null
        constraint memberships_status_check check (status in ('pending', 'joined')),
    via              text not null
        constraint memberships_via_check check (via in ('direct', 'request', 'invite')),
    invitation_id    uuid references public.invitations (id) on delete set null,
    reviewed_by      uuid references public.profiles (id) on delete set null,
    welcome_seen_at  timestamptz,
    created_at       timestamptz not null default now(),
    constraint memberships_community_user_key unique (community_id, user_id),
    constraint memberships_reviewer_not_self check (reviewed_by <> user_id),
    constraint memberships_reviewed_only_requests check (reviewed_by is null or via = 'request'),
    constraint memberships_invitation_only_invites check (invitation_id is null or via = 'invite'),
    constraint memberships_pending_is_member check (status = 'joined' or role = 'member'),
    constraint memberships_pending_only_requests check (status = 'joined' or via = 'request')
);

-- Exactly one owner per community: at most one here, at least one by trigger.
create unique index memberships_one_owner_key on public.memberships (community_id) where role = 'owner';
create index memberships_user_id_idx on public.memberships (user_id);

create table public.community_questions (
    id            uuid primary key default gen_random_uuid(),
    community_id  uuid not null references public.communities (id) on delete cascade,
    label         varchar(200) not null
        constraint community_questions_label_not_blank check (btrim(label) <> ''),
    kind          text not null
        constraint community_questions_kind_check check (kind in
            ('short_text', 'long_text', 'single_choice', 'multi_choice', 'number', 'date', 'url')),
    why_we_ask    varchar(200),
    required      boolean not null default false,
    options       jsonb,
    position      integer not null default 0
        constraint community_questions_position_nonnegative check (position >= 0),
    constraint community_questions_options_check check (
        case when kind in ('single_choice', 'multi_choice')
             then options is not null and jsonb_typeof(options) = 'array' and jsonb_array_length(options) >= 1
             else options is null
        end)
);

create index community_questions_community_id_idx on public.community_questions (community_id, position);

create table public.join_answers (
    id             uuid primary key default gen_random_uuid(),
    membership_id  uuid not null references public.memberships (id) on delete cascade,
    question_id    uuid not null references public.community_questions (id) on delete cascade,
    value          varchar(2000) not null
        constraint join_answers_value_not_blank check (btrim(value) <> ''),
    constraint join_answers_membership_question_key unique (membership_id, question_id)
);

create index join_answers_question_id_idx on public.join_answers (question_id);

-- ---------------------------------------------------------------------------
-- Helper functions (SECURITY DEFINER so policies can use them without
-- recursing through memberships' own RLS).
-- ---------------------------------------------------------------------------

create or replace function public.is_joined(p_community_id uuid)
returns boolean
language sql stable security definer set search_path = ''
as $$
  select exists (
    select 1 from public.memberships m
    where m.community_id = p_community_id
      and m.user_id = auth.uid()
      and m.status = 'joined'
  )
$$;

-- The caller's role in a community, or null unless joined.
create or replace function public.community_role(p_community_id uuid)
returns text
language sql stable security definer set search_path = ''
as $$
  select m.role from public.memberships m
  where m.community_id = p_community_id
    and m.user_id = auth.uid()
    and m.status = 'joined'
$$;

create or replace function public.has_community_role(p_community_id uuid, p_roles text[])
returns boolean
language sql stable security definer set search_path = ''
as $$
  select coalesce(public.community_role(p_community_id) = any (p_roles), false)
$$;

-- Whether the caller may issue invitations, per the community's invite_permission.
create or replace function public.can_invite(p_community_id uuid)
returns boolean
language sql stable security definer set search_path = ''
as $$
  select coalesce((
    select case c.invite_permission
             when 'admins' then r.role in ('owner', 'admin')
             when 'admins_moderators' then r.role in ('owner', 'admin', 'moderator')
             else true
           end
    from public.communities c
    cross join lateral (select public.community_role(c.id) as role) r
    where c.id = p_community_id and c.deleted_at is null and r.role is not null
  ), false)
$$;

-- Whether the caller has any membership row (pending or joined).
create or replace function public.has_membership(p_community_id uuid)
returns boolean
language sql stable security definer set search_path = ''
as $$
  select exists (
    select 1 from public.memberships m
    where m.community_id = p_community_id and m.user_id = auth.uid()
  )
$$;

-- Whether the caller and another user are both joined to some community.
create or replace function public.shares_community(p_user_id uuid)
returns boolean
language sql stable security definer set search_path = ''
as $$
  select exists (
    select 1
    from public.memberships mine
    join public.memberships theirs on theirs.community_id = mine.community_id
    where mine.user_id = auth.uid() and mine.status = 'joined'
      and theirs.user_id = p_user_id and theirs.status = 'joined'
  )
$$;

create or replace function public.is_operator()
returns boolean
language sql stable security definer set search_path = ''
as $$
  select coalesce((select p.is_operator from public.profiles p where p.id = auth.uid()), false)
$$;

-- ---------------------------------------------------------------------------
-- Triggers
-- ---------------------------------------------------------------------------

create or replace function public.touch_updated_at()
returns trigger
language plpgsql set search_path = ''
as $$
begin
  new.updated_at := now();
  return new;
end
$$;

create trigger profiles_touch_updated_at before update on public.profiles
  for each row execute function public.touch_updated_at();
create trigger communities_touch_updated_at before update on public.communities
  for each row execute function public.touch_updated_at();

-- member_count = number of joined memberships.
create or replace function public.memberships_maintain_member_count()
returns trigger
language plpgsql security definer set search_path = ''
as $$
begin
  if tg_op in ('UPDATE', 'DELETE') and old.status = 'joined' then
    update public.communities set member_count = member_count - 1 where id = old.community_id;
  end if;
  if tg_op in ('INSERT', 'UPDATE') and new.status = 'joined' then
    update public.communities set member_count = member_count + 1 where id = new.community_id;
  end if;
  return null;
end
$$;

create trigger memberships_member_count
  after insert or update of status, community_id or delete on public.memberships
  for each row execute function public.memberships_maintain_member_count();

-- At commit, every live community has its owner membership. Catches a
-- community created without one and an owner removed outside a transfer.
create or replace function public.communities_require_owner()
returns trigger
language plpgsql security definer set search_path = ''
as $$
declare
  v_community_id uuid;
begin
  if tg_table_name = 'communities' then
    v_community_id := new.id;
  else
    v_community_id := old.community_id;
  end if;
  if exists (select 1 from public.communities c where c.id = v_community_id)
     and not exists (
       select 1 from public.memberships m
       where m.community_id = v_community_id and m.role = 'owner' and m.status = 'joined'
     ) then
    raise exception 'community % must have exactly one owner', v_community_id
      using errcode = '23514';
  end if;
  return null;
end
$$;

create constraint trigger communities_owner_required
  after insert on public.communities
  deferrable initially deferred
  for each row execute function public.communities_require_owner();

create constraint trigger memberships_owner_required
  after update of role, status or delete on public.memberships
  deferrable initially deferred
  for each row when (old.role = 'owner')
  execute function public.communities_require_owner();

-- ---------------------------------------------------------------------------
-- Row level security and privileges
-- ---------------------------------------------------------------------------

alter table public.profiles enable row level security;
alter table public.communities enable row level security;
alter table public.memberships enable row level security;
alter table public.community_questions enable row level security;
alter table public.join_answers enable row level security;
alter table public.invitations enable row level security;

revoke all on public.profiles, public.communities, public.memberships,
              public.community_questions, public.join_answers, public.invitations
  from anon, authenticated;

grant select on public.profiles, public.communities, public.memberships,
                public.community_questions, public.join_answers, public.invitations
  to anon, authenticated;
-- Policies below are for "authenticated" only, so visitors (anon) read zero rows.

-- Profiles: own row, or anyone sharing a joined community.
create policy profiles_select on public.profiles for select to authenticated
  using (id = auth.uid() or public.shares_community(id));

-- Self-service profile fields only; server-set columns are not granted.
grant insert (id, username, full_name, headline, company, city, bio, avatar_url, skills, interests,
              onboarding_complete, profile_visibility, appear_in_search, hide_city, activity_status,
              login_alerts)
  on public.profiles to authenticated;
grant update (username, full_name, headline, company, city, bio, avatar_url, skills, interests,
              onboarding_complete, profile_visibility, appear_in_search, hide_city, activity_status,
              login_alerts)
  on public.profiles to authenticated;

create policy profiles_insert_own on public.profiles for insert to authenticated
  with check (id = auth.uid());
create policy profiles_update_own on public.profiles for update to authenticated
  using (id = auth.uid()) with check (id = auth.uid());

-- Communities: live and public, or the caller has a membership row
-- (pending members see the overview). Writes go through RPCs.
create policy communities_select on public.communities for select to authenticated
  using (deleted_at is null and (visibility = 'public' or public.has_membership(id)));

-- Memberships: own rows; joined members see the joined roster; owners and
-- admins also see pending requests. Inserts and role/status changes only
-- through RPCs.
create policy memberships_select on public.memberships for select to authenticated
  using (
    user_id = auth.uid()
    or (status = 'joined' and public.is_joined(community_id))
    or public.has_community_role(community_id, array['owner', 'admin'])
  );

grant update (welcome_seen_at) on public.memberships to authenticated;
create policy memberships_update_own on public.memberships for update to authenticated
  using (user_id = auth.uid()) with check (user_id = auth.uid());

-- Leave a community or withdraw a request; the owner must transfer first.
grant delete on public.memberships to authenticated;
create policy memberships_delete_own on public.memberships for delete to authenticated
  using (user_id = auth.uid() and role <> 'owner');

-- Questions: visible wherever the community is visible.
create policy community_questions_select on public.community_questions for select to authenticated
  using (exists (
    select 1 from public.communities c
    where c.id = community_id and c.deleted_at is null
      and (c.visibility = 'public' or public.has_membership(c.id))
  ));

-- Answers: the applicant, and the community's owner and admins.
create policy join_answers_select on public.join_answers for select to authenticated
  using (exists (
    select 1 from public.memberships m
    where m.id = membership_id
      and (m.user_id = auth.uid() or public.has_community_role(m.community_id, array['owner', 'admin']))
  ));

-- Invitations: members allowed to invite. The public landing uses open_invitation().
create policy invitations_select on public.invitations for select to authenticated
  using (public.can_invite(community_id));

-- ---------------------------------------------------------------------------
-- RPCs
-- ---------------------------------------------------------------------------

-- Validates and stores join answers for a membership.
-- p_answers: [{"question_id": uuid, "value": text}, ...]
create or replace function public._save_join_answers(p_membership_id uuid, p_community_id uuid, p_answers jsonb)
returns void
language plpgsql security definer set search_path = ''
as $$
declare
  v_missing text;
begin
  p_answers := coalesce(p_answers, '[]'::jsonb);
  if jsonb_typeof(p_answers) <> 'array' then
    raise exception 'Answers must be a list.' using errcode = 'PT400';
  end if;

  if exists (
    select 1 from jsonb_array_elements(p_answers) a
    where not exists (
      select 1 from public.community_questions q
      where q.id = (a ->> 'question_id')::uuid and q.community_id = p_community_id
    )
  ) then
    raise exception 'An answer refers to an unknown question.' using errcode = 'PT400';
  end if;

  select q.label into v_missing
  from public.community_questions q
  where q.community_id = p_community_id and q.required
    and not exists (
      select 1 from jsonb_array_elements(p_answers) a
      where (a ->> 'question_id')::uuid = q.id and btrim(coalesce(a ->> 'value', '')) <> ''
    )
  order by q.position
  limit 1;
  if v_missing is not null then
    raise exception 'Answer the required question: %', v_missing using errcode = 'PT400';
  end if;

  insert into public.join_answers (membership_id, question_id, value)
  select p_membership_id, (a ->> 'question_id')::uuid, a ->> 'value'
  from jsonb_array_elements(p_answers) a
  where btrim(coalesce(a ->> 'value', '')) <> ''
  on conflict (membership_id, question_id) do update set value = excluded.value;
end
$$;

-- Creates a community and its owner membership in one transaction.
-- Idempotent on p_creation_key: a repeat call returns the existing row.
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
  v_n integer := 1;
begin
  if v_uid is null then
    raise exception 'Sign in to create a community.' using errcode = 'PT401';
  end if;

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

  v_base := left(btrim(regexp_replace(lower(btrim(p_name)), '[^a-z0-9]+', '-', 'g'), '-'), 34);
  v_base := btrim(v_base, '-');
  if v_base = '' then
    v_base := 'community';
  end if;
  v_handle := v_base;
  while exists (select 1 from public.communities where handle = v_handle) loop
    v_n := v_n + 1;
    v_handle := v_base || '-' || v_n;
  end loop;

  insert into public.communities (handle, name, location_type, city, visibility, created_by, creation_key)
  values (v_handle, btrim(p_name), p_location_type, nullif(btrim(coalesce(p_city, '')), ''),
          coalesce(p_visibility, 'private'), v_uid, p_creation_key)
  returning * into v_row;

  insert into public.memberships (community_id, user_id, role, status, via)
  values (v_row.id, v_uid, 'owner', 'joined', 'direct');

  select * into v_row from public.communities where id = v_row.id;
  return v_row;
end
$$;

-- Makes a joined member Owner and demotes the previous owner to Admin.
create or replace function public.transfer_ownership(p_community_id uuid, p_to_user_id uuid)
returns public.memberships
language plpgsql security definer set search_path = ''
as $$
declare
  v_uid uuid := auth.uid();
  v_row public.memberships;
begin
  if not public.has_community_role(p_community_id, array['owner']) then
    raise exception 'Only the owner can transfer ownership.' using errcode = 'PT403';
  end if;
  if p_to_user_id = v_uid or not exists (
    select 1 from public.memberships
    where community_id = p_community_id and user_id = p_to_user_id and status = 'joined'
  ) then
    raise exception 'The new owner must be a joined member.' using errcode = 'PT422';
  end if;

  update public.memberships set role = 'admin'
  where community_id = p_community_id and user_id = v_uid;
  update public.memberships set role = 'owner'
  where community_id = p_community_id and user_id = p_to_user_id
  returning * into v_row;
  return v_row;
end
$$;

-- Submits a join request to a public community; creates a pending membership.
create or replace function public.request_join(p_community_id uuid, p_answers jsonb default '[]'::jsonb)
returns public.memberships
language plpgsql security definer set search_path = ''
as $$
declare
  v_uid uuid := auth.uid();
  v_visibility text;
  v_row public.memberships;
begin
  if v_uid is null then
    raise exception 'Sign in to join.' using errcode = 'PT401';
  end if;
  select visibility into v_visibility from public.communities
  where id = p_community_id and deleted_at is null;
  if not found then
    raise exception 'Community not found.' using errcode = 'PT404';
  end if;
  if v_visibility <> 'public' then
    raise exception 'This community is invite only.' using errcode = 'PT403';
  end if;
  if exists (select 1 from public.memberships where community_id = p_community_id and user_id = v_uid) then
    raise exception 'You have already requested to join or are a member.' using errcode = 'PT409';
  end if;

  insert into public.memberships (community_id, user_id, role, status, via)
  values (p_community_id, v_uid, 'member', 'pending', 'request')
  returning * into v_row;
  perform public._save_join_answers(v_row.id, p_community_id, p_answers);
  return v_row;
end
$$;

-- Approves or rejects a pending request. Joined owners/admins only, never
-- their own request. Reject deletes the row and returns it.
create or replace function public.review_join_request(p_community_id uuid, p_user_id uuid, p_decision text)
returns public.memberships
language plpgsql security definer set search_path = ''
as $$
declare
  v_uid uuid := auth.uid();
  v_row public.memberships;
begin
  if p_decision not in ('approve', 'reject') then
    raise exception 'decision must be approve or reject' using errcode = 'PT400';
  end if;
  if not public.has_community_role(p_community_id, array['owner', 'admin']) or p_user_id = v_uid then
    raise exception 'Admins only.' using errcode = 'PT403';
  end if;

  select * into v_row from public.memberships
  where community_id = p_community_id and user_id = p_user_id and status = 'pending'
  for update;
  if not found then
    raise exception 'No pending request.' using errcode = 'PT404';
  end if;

  if p_decision = 'approve' then
    update public.memberships set status = 'joined', reviewed_by = v_uid
    where id = v_row.id returning * into v_row;
  else
    delete from public.memberships where id = v_row.id;
  end if;
  return v_row;
end
$$;

-- Locks and validates an invitation for p_community_id/p_code.
-- Raises PT404 when the code does not exist for this community and PT410 when
-- it is expired, revoked or used up.
create or replace function public._usable_invitation(p_community_id uuid, p_code text)
returns public.invitations
language plpgsql security definer set search_path = ''
as $$
declare
  v_inv public.invitations;
begin
  select i.* into v_inv
  from public.invitations i
  join public.communities c on c.id = i.community_id and c.deleted_at is null
  where i.code = p_code and i.community_id = p_community_id
  for update of i;
  if not found then
    raise exception 'This invitation is not valid. Ask a community admin for an invite.' using errcode = 'PT404';
  end if;
  if v_inv.revoked_at is not null or v_inv.expires_at <= now() or v_inv.use_count >= v_inv.max_uses then
    raise exception 'This invitation is not valid. Ask a community admin for an invite.' using errcode = 'PT410';
  end if;
  return v_inv;
end
$$;

-- Public invite landing: validates the code, records the open and returns a
-- community preview plus its join questions.
create or replace function public.open_invitation(p_community_id uuid, p_code text)
returns jsonb
language plpgsql security definer set search_path = ''
as $$
declare
  v_inv public.invitations;
begin
  begin
    v_inv := public._usable_invitation(p_community_id, p_code);
  exception when sqlstate 'PT410' then
    -- The landing page shows every unusable code the same way.
    raise exception 'This invitation is not valid. Ask a community admin for an invite.' using errcode = 'PT404';
  end;

  update public.invitations set tracking_state = 'opened'
  where id = v_inv.id and tracking_state = 'sent';
  select * into v_inv from public.invitations where id = v_inv.id;

  return jsonb_build_object(
    'invitation', jsonb_build_object(
      'id', v_inv.id, 'community_id', v_inv.community_id, 'code', v_inv.code,
      'channel', v_inv.channel, 'expires_at', v_inv.expires_at, 'max_uses', v_inv.max_uses,
      'use_count', v_inv.use_count, 'tracking_state', v_inv.tracking_state),
    'community', (
      select jsonb_build_object(
        'id', c.id, 'handle', c.handle, 'name', c.name, 'category', c.category,
        'city', c.city, 'location_type', c.location_type, 'visibility', c.visibility,
        'emoji', c.emoji, 'cover_gradient', c.cover_gradient, 'logo_url', c.logo_url,
        'cover_url', c.cover_url, 'member_count', c.member_count)
      from public.communities c where c.id = v_inv.community_id),
    'questions', coalesce((
      select jsonb_agg(jsonb_build_object(
        'id', q.id, 'label', q.label, 'kind', q.kind, 'why_we_ask', q.why_we_ask,
        'required', q.required, 'options', q.options, 'position', q.position) order by q.position)
      from public.community_questions q where q.community_id = v_inv.community_id), '[]'::jsonb)
  );
end
$$;

-- Accepts an invitation: the caller joins via invite. An existing joined
-- membership is returned unchanged; a pending request is admitted.
create or replace function public.accept_invitation(p_community_id uuid, p_code text, p_answers jsonb default '[]'::jsonb)
returns public.memberships
language plpgsql security definer set search_path = ''
as $$
declare
  v_uid uuid := auth.uid();
  v_inv public.invitations;
  v_row public.memberships;
begin
  if v_uid is null then
    raise exception 'Sign in to accept the invitation.' using errcode = 'PT401';
  end if;
  v_inv := public._usable_invitation(p_community_id, p_code);
  if v_inv.channel = 'in_app' and v_inv.invitee_user_id <> v_uid then
    raise exception 'This invitation is not valid. Ask a community admin for an invite.' using errcode = 'PT404';
  end if;

  select * into v_row from public.memberships
  where community_id = p_community_id and user_id = v_uid
  for update;
  if found and v_row.status = 'joined' then
    return v_row;
  end if;

  if found then
    delete from public.join_answers where membership_id = v_row.id;
    update public.memberships
    set status = 'joined', via = 'invite', invitation_id = v_inv.id, reviewed_by = null
    where id = v_row.id returning * into v_row;
  else
    insert into public.memberships (community_id, user_id, role, status, via, invitation_id)
    values (p_community_id, v_uid, 'member', 'joined', 'invite', v_inv.id)
    returning * into v_row;
  end if;
  perform public._save_join_answers(v_row.id, p_community_id, p_answers);

  update public.invitations
  set use_count = use_count + 1, tracking_state = 'accepted'
  where id = v_inv.id;
  return v_row;
end
$$;

-- Execute rights: helpers for policies, RPCs for signed-in users,
-- open_invitation for visitors too. Internal helpers stay private.
revoke all on function
  public.is_joined(uuid), public.community_role(uuid), public.has_community_role(uuid, text[]),
  public.can_invite(uuid), public.has_membership(uuid), public.shares_community(uuid),
  public.is_operator(), public._save_join_answers(uuid, uuid, jsonb),
  public._usable_invitation(uuid, text), public.create_community(text, text, uuid, text, text),
  public.transfer_ownership(uuid, uuid), public.request_join(uuid, jsonb),
  public.review_join_request(uuid, uuid, text), public.open_invitation(uuid, text),
  public.accept_invitation(uuid, text, jsonb),
  public.memberships_maintain_member_count(), public.communities_require_owner()
  from public, anon, authenticated;

grant execute on function
  public.is_joined(uuid), public.community_role(uuid), public.has_community_role(uuid, text[]),
  public.can_invite(uuid), public.has_membership(uuid), public.shares_community(uuid),
  public.is_operator(), public.create_community(text, text, uuid, text, text),
  public.transfer_ownership(uuid, uuid), public.request_join(uuid, jsonb),
  public.review_join_request(uuid, uuid, text), public.accept_invitation(uuid, text, jsonb),
  public.open_invitation(uuid, text)
  to authenticated, service_role;
grant execute on function public.open_invitation(uuid, text) to anon;
