-- Jobs: full Opportunity/Looking jobs, job interests (express interest /
-- ask for referral), and the jobs_feed view that merges jobs with Home posts
-- tagged Available job / Need job (one view for the main and community Jobs
-- routes; retagging or deleting a post moves it out without copies).
-- Also finishes bookmarks.job_id (FK + policies) left open by the feed migration.

-- ---------------------------------------------------------------------------
-- Tables
-- ---------------------------------------------------------------------------

create table public.jobs (
    id            uuid primary key default gen_random_uuid(),
    community_id  uuid not null references public.communities (id) on delete cascade,
    poster_id     uuid not null default auth.uid() references public.profiles (id) on delete cascade,
    intent        text not null
        constraint jobs_intent_check check (intent in ('opportunity', 'looking')),
    -- 'Add a role or title to post'
    title         varchar(120) not null constraint jobs_title_check check (title ~ '\S'),
    company       varchar(100),
    location      varchar(100),
    job_type      text
        constraint jobs_job_type_check check (job_type in ('full_time', 'part_time', 'internship', 'contract', 'freelance')),
    work_mode     text
        constraint jobs_work_mode_check check (work_mode in ('on_site', 'remote', 'hybrid')),
    experience    varchar(60),
    salary        varchar(60),
    -- http(s) only; null hides Apply
    apply_url     text constraint jobs_apply_url_http check (apply_url is null or public._is_http_url(apply_url)),
    details       varchar(4000),
    deleted_at    timestamptz,
    created_at    timestamptz not null default now(),
    updated_at    timestamptz not null default now()
);

-- Jobs list: one community, one intent, newest/oldest, live rows only.
create index jobs_list_idx on public.jobs (community_id, intent, created_at desc, id desc)
  where deleted_at is null;
create index jobs_poster_idx on public.jobs (poster_id, created_at desc);

create table public.job_interests (
    id          uuid primary key default gen_random_uuid(),
    job_id      uuid not null references public.jobs (id) on delete cascade,
    user_id     uuid not null default auth.uid() references public.profiles (id) on delete cascade,
    kind        text not null constraint job_interests_kind_check check (kind in ('interest', 'referral')),
    note        varchar(280),
    -- PDF or DOCX only (storage path or URL; a query string is tolerated)
    resume_url  text constraint job_interests_resume_check
        check (resume_url is null or resume_url ~* '\.(pdf|docx)(\?.*)?$'),
    replied_at  timestamptz,
    created_at  timestamptz not null default now(),
    constraint job_interests_job_user_kind_key unique (job_id, user_id, kind)
);

-- Profile → Activity lists the caller's requests newest first.
create index job_interests_user_idx on public.job_interests (user_id, created_at desc);

-- Job-tagged Home posts: few rows among all posts; jobs_feed reaches them
-- through this partial index and joins post_targets on (post_id, community_id).
create index posts_job_tag_idx on public.posts (created_at desc, id)
  where job_tag <> 'general' and deleted_at is null;

-- Bookmarks may now point at jobs.
alter table public.bookmarks add constraint bookmarks_job_id_fkey
  foreign key (job_id) references public.jobs (id) on delete cascade;

-- ---------------------------------------------------------------------------
-- Access helpers
-- ---------------------------------------------------------------------------

-- A live job in a live community the caller is joined to.
create or replace function public.can_see_job(p_job_id uuid)
returns boolean
language sql stable security definer set search_path = ''
as $$
  select exists (
    select 1
    from public.jobs j
    join public.communities c on c.id = j.community_id and c.deleted_at is null
    join public.memberships m on m.community_id = j.community_id
      and m.user_id = auth.uid() and m.status = 'joined'
    where j.id = p_job_id and j.deleted_at is null
  )
$$;

create or replace function public._job_live(p_job_id uuid)
returns boolean
language sql stable security definer set search_path = ''
as $$
  select exists (select 1 from public.jobs j where j.id = p_job_id and j.deleted_at is null)
$$;

create or replace function public.is_job_poster(p_job_id uuid)
returns boolean
language sql stable security definer set search_path = ''
as $$
  select exists (select 1 from public.jobs j where j.id = p_job_id and j.poster_id = auth.uid())
$$;

-- ---------------------------------------------------------------------------
-- Triggers
-- ---------------------------------------------------------------------------

-- Insert: the poster must be joined to a live community; server-set columns.
create or replace function public.jobs_before_insert()
returns trigger
language plpgsql security definer set search_path = ''
as $$
begin
  if not exists (
    select 1 from public.memberships m
    join public.communities c on c.id = m.community_id and c.deleted_at is null
    where m.community_id = new.community_id and m.user_id = new.poster_id and m.status = 'joined'
  ) then
    raise exception 'You can only post jobs to communities you have joined.' using errcode = 'PT403';
  end if;
  new.deleted_at := null;
  new.created_at := now();
  new.updated_at := new.created_at;
  return new;
end
$$;

create trigger jobs_before_insert before insert on public.jobs
  for each row execute function public.jobs_before_insert();

-- Update: poster, community and created_at are fixed; a deletion is final.
create or replace function public.jobs_before_update()
returns trigger
language plpgsql set search_path = ''
as $$
begin
  new.poster_id := old.poster_id;
  new.community_id := old.community_id;
  new.created_at := old.created_at;
  if old.deleted_at is not null then
    new.deleted_at := old.deleted_at;
  end if;
  new.updated_at := now();
  return new;
end
$$;

create trigger jobs_before_update before update on public.jobs
  for each row execute function public.jobs_before_update();

create or replace function public.job_interests_before_insert()
returns trigger
language plpgsql set search_path = ''
as $$
begin
  new.replied_at := null;
  new.created_at := now();
  return new;
end
$$;

create trigger job_interests_before_insert before insert on public.job_interests
  for each row execute function public.job_interests_before_insert();

-- ---------------------------------------------------------------------------
-- jobs_feed: jobs ∪ job-tagged posts, per community (DEC-005)
-- ---------------------------------------------------------------------------
-- Owner rights + security_barrier (like raises_masked): filters by the caller
-- itself (inline checks, so anon gets 0 rows, not 42501) — joined members of a live community only, live rows only, blocks
-- either way excluded. Query it with `where community_id = $1 and intent = $2`;
-- the main and community Jobs routes run the same query.
--   item_kind   'job' (row in jobs) | 'post' (Home post tagged available/need)
--   has_details true for jobs; false for posts — no /jobs/:id page, no Apply
--   intent      jobs.intent, or available → opportunity, need → looking
--   title       job title, or the post body flattened to one line (≤120)
--   details     job details, or the full post body
create view public.jobs_feed with (security_barrier = true) as
select
  'job'::text                      as item_kind,
  j.id, j.community_id, j.poster_id, j.intent, j.title::text as title,
  j.company::text as company, j.location::text as location, j.job_type, j.work_mode,
  j.experience::text as experience, j.salary::text as salary, j.apply_url,
  j.details::text as details,
  true                             as has_details,
  null::uuid                       as post_id,
  j.created_at, j.updated_at,
  pr.full_name::text               as poster_name,
  pr.avatar_url                    as poster_avatar_url
from public.jobs j
join public.communities c on c.id = j.community_id and c.deleted_at is null
join public.profiles pr on pr.id = j.poster_id
where j.deleted_at is null
  and exists (select 1 from public.memberships m
    where m.community_id = j.community_id and m.user_id = auth.uid() and m.status = 'joined')
  and not exists (select 1 from public.blocks b
    where (b.blocker_id = auth.uid() and b.blocked_id = j.poster_id)
       or (b.blocker_id = j.poster_id and b.blocked_id = auth.uid()))
union all
select
  'post'::text,
  p.id, t.community_id, p.author_id,
  case p.job_tag when 'available' then 'opportunity' else 'looking' end,
  left(regexp_replace(btrim(coalesce(p.body, '')), '\s+', ' ', 'g'), 120),
  null, null, null, null, null, null, null,
  p.body::text,
  false,
  p.id,
  t.created_at, coalesce(p.edited_at, p.created_at),
  pr.full_name::text,
  pr.avatar_url
from public.posts p
join public.post_targets t on t.post_id = p.id
join public.communities c on c.id = t.community_id and c.deleted_at is null
join public.profiles pr on pr.id = p.author_id
where p.job_tag in ('available', 'need')
  and p.deleted_at is null
  and exists (select 1 from public.memberships m
    where m.community_id = t.community_id and m.user_id = auth.uid() and m.status = 'joined')
  and not exists (select 1 from public.blocks b
    where (b.blocker_id = auth.uid() and b.blocked_id = p.author_id)
       or (b.blocker_id = p.author_id and b.blocked_id = auth.uid()));

-- ---------------------------------------------------------------------------
-- RLS and grants
-- ---------------------------------------------------------------------------

alter table public.jobs enable row level security;
alter table public.job_interests enable row level security;

revoke all on public.jobs, public.job_interests, public.jobs_feed from anon, authenticated;

-- jobs: joined members read live jobs; a joined member posts (insert under
-- RLS); the poster edits the client columns. Delete = delete_job() (soft).
grant select on public.jobs to anon, authenticated;
grant insert (community_id, intent, title, company, location, job_type, work_mode,
              experience, salary, apply_url, details) on public.jobs to authenticated;
grant update (intent, title, company, location, job_type, work_mode,
              experience, salary, apply_url, details) on public.jobs to authenticated;
create policy jobs_select_joined on public.jobs for select to authenticated
  using (deleted_at is null and public.is_joined(community_id));
create policy jobs_insert_member on public.jobs for insert to authenticated
  with check (poster_id = auth.uid() and public.is_joined(community_id));
create policy jobs_update_poster on public.jobs for update to authenticated
  using (poster_id = auth.uid() and deleted_at is null)
  with check (poster_id = auth.uid() and deleted_at is null);

-- job_interests: the requester (Profile → Activity) and the job's poster read;
-- inserts and replies go through submit_job_interest / reply_job_interest.
grant select on public.job_interests to anon, authenticated;
create policy job_interests_select on public.job_interests for select to authenticated
  using (user_id = auth.uid() or public.is_job_poster(job_id));

grant select on public.jobs_feed to anon, authenticated;

-- bookmarks: posts or live, visible jobs; Saved hides deleted posts and jobs.
drop policy bookmarks_select_own on public.bookmarks;
drop policy bookmarks_insert_own on public.bookmarks;
create policy bookmarks_select_own on public.bookmarks for select to authenticated
  using (user_id = auth.uid()
    and (post_id is null or public._post_live(post_id))
    and (job_id is null or public._job_live(job_id)));
create policy bookmarks_insert_own on public.bookmarks for insert to authenticated
  with check (user_id = auth.uid() and (
    (post_id is not null and public.can_see_post(post_id))
    or (job_id is not null and public.can_see_job(job_id))));

-- ---------------------------------------------------------------------------
-- RPCs (authenticated, definer)
-- ---------------------------------------------------------------------------

-- Soft-delete one's own job: PT404 unknown/deleted/not visible, PT403 not
-- the poster. The job leaves jobs_feed, Saved and getJob at once.
create or replace function public.delete_job(p_job_id uuid)
returns public.jobs
language plpgsql security definer set search_path = ''
as $$
declare
  v_job public.jobs;
begin
  select * into v_job from public.jobs j where j.id = p_job_id and j.deleted_at is null for update;
  if not found or not public.is_joined(v_job.community_id) then
    raise exception 'Job not found' using errcode = 'PT404';
  end if;
  if v_job.poster_id is distinct from auth.uid() then
    raise exception 'Only the poster can delete this job.' using errcode = 'PT403';
  end if;
  update public.jobs set deleted_at = now() where id = p_job_id returning * into v_job;
  return v_job;
end
$$;

-- Express interest or ask for a referral on a visible job. Stores the row
-- and a 'job_interest' notification for the poster
-- {job_interest_id, job_id, requester_id, kind}. PT404 job not visible,
-- PT400 bad kind / own job, 22001 note > 280, 23514 resume not PDF/DOCX,
-- 23505 same kind already sent (→ 409). Returns the row as jsonb.
create or replace function public.submit_job_interest(
  p_job_id uuid, p_kind text, p_note text default null, p_resume_url text default null)
returns jsonb
language plpgsql security definer set search_path = ''
as $$
declare
  v_uid uuid := auth.uid();
  v_job public.jobs;
  v_row public.job_interests;
begin
  if v_uid is null then
    raise exception 'Sign in to continue.' using errcode = 'PT401';
  end if;
  if p_kind is null or p_kind not in ('interest', 'referral') then
    raise exception 'Choose interest or referral.' using errcode = 'PT400';
  end if;
  if not public.can_see_job(p_job_id) then
    raise exception 'Job not found' using errcode = 'PT404';
  end if;
  select * into v_job from public.jobs j where j.id = p_job_id;
  if v_job.poster_id = v_uid then
    raise exception 'This is your own job post.' using errcode = 'PT400';
  end if;
  if public.blocked_between(v_uid, v_job.poster_id) then
    raise exception 'Job not found' using errcode = 'PT404';
  end if;
  insert into public.job_interests (job_id, user_id, kind, note, resume_url)
  values (p_job_id, v_uid, p_kind, nullif(btrim(p_note), ''), nullif(btrim(p_resume_url), ''))
  returning * into v_row;
  perform public.notify(v_job.poster_id, 'job_interest', jsonb_build_object(
    'job_interest_id', v_row.id, 'job_id', p_job_id, 'requester_id', v_uid, 'kind', p_kind));
  return to_jsonb(v_row);
end
$$;

-- The poster replies to an interest: stamps replied_at (first reply kept)
-- and stores a 'job_interest_reply' notification for the requester
-- {job_interest_id, job_id, poster_id, kind} on the first reply only.
-- PT404 unknown / not visible, PT403 not the poster.
create or replace function public.reply_job_interest(p_interest_id uuid)
returns jsonb
language plpgsql security definer set search_path = ''
as $$
declare
  v_row public.job_interests;
  v_job public.jobs;
begin
  select * into v_row from public.job_interests i where i.id = p_interest_id for update;
  if not found then
    raise exception 'Request not found' using errcode = 'PT404';
  end if;
  select * into v_job from public.jobs j where j.id = v_row.job_id;
  if v_job.poster_id is distinct from auth.uid() then
    if v_row.user_id = auth.uid() then
      raise exception 'Only the poster can reply.' using errcode = 'PT403';
    end if;
    raise exception 'Request not found' using errcode = 'PT404';
  end if;
  if v_row.replied_at is null then
    update public.job_interests set replied_at = now() where id = p_interest_id returning * into v_row;
    perform public.notify(v_row.user_id, 'job_interest_reply', jsonb_build_object(
      'job_interest_id', v_row.id, 'job_id', v_row.job_id, 'poster_id', v_job.poster_id, 'kind', v_row.kind));
  end if;
  return to_jsonb(v_row);
end
$$;

-- ---------------------------------------------------------------------------
-- Execute rights
-- ---------------------------------------------------------------------------

revoke all on function
  public.can_see_job(uuid), public._job_live(uuid), public.is_job_poster(uuid),
  public.jobs_before_insert(), public.jobs_before_update(), public.job_interests_before_insert(),
  public.delete_job(uuid), public.submit_job_interest(uuid, text, text, text),
  public.reply_job_interest(uuid)
  from public, anon, authenticated;

grant execute on function
  public.can_see_job(uuid), public._job_live(uuid), public.is_job_poster(uuid),
  public.delete_job(uuid), public.submit_job_interest(uuid, text, text, text),
  public.reply_job_interest(uuid)
  to authenticated, service_role;
