-- Engagement API: likes, comments, poll votes, bookmarks and content filters
-- (OP-034..043). All RPCs are SECURITY DEFINER, executable by authenticated,
-- and act for auth.uid(). Visibility = _post_visible (can_see_post and no
-- block either way with the author) → otherwise PT404 'Post not found'.
--   like_target(post_id, comment_id) → like row + like_count (idempotent)
--   unlike(id) → bool (id = like id, post id or comment id; own likes only)
--   list_comments(post_id, sort relevant|recent) → jsonb tree (replies nested)
--   create_comment(post_id, parent_id, body) → _comment_json
--   vote_poll(post_id, option_index) → vote row + my_vote + poll_results
--   save_bookmark(post_id, job_id) → bookmark row (idempotent)
--   remove_bookmark(id) → bool (bookmark id, post id or job id)
--   list_bookmarks() → saved live, visible posts and jobs, newest first
--   add_content_filter(kind, post_id, muted_user_id) → row (idempotent)
--   remove_content_filter(id) → bool (filter id, post id or muted user id)
-- like_count changes are written by the likes_count trigger to posts /
-- comments, which are in publication supabase_realtime (live counts).

-- Visible post: live, in a joined live community, author not blocked.
create or replace function public._post_visible(p_post_id uuid)
returns boolean
language sql stable security definer set search_path = ''
as $$
  select public.can_see_post(p_post_id) and not exists (
    select 1 from public.posts p
    where p.id = p_post_id and public.blocked_between(auth.uid(), p.author_id))
$$;

-- One comment with its author card, liked/mine flags and nested replies
-- (oldest first; replies by blocked users are left out).
create or replace function public._comment_json(c public.comments)
returns jsonb
language sql stable security definer set search_path = ''
as $$
  select to_jsonb(c) || jsonb_build_object(
    'author', public._profile_card(c.author_id),
    'liked', exists (select 1 from public.likes l where l.comment_id = c.id and l.user_id = auth.uid()),
    'mine', c.author_id = auth.uid(),
    'replies', coalesce((
      select jsonb_agg(public._comment_json(r) order by r.created_at, r.id)
      from public.comments r
      where r.parent_id = c.id
        and not public.blocked_between(auth.uid(), r.author_id)), '[]'::jsonb))
$$;

create or replace function public.like_target(p_post_id uuid, p_comment_id uuid)
returns jsonb
language plpgsql security definer set search_path = ''
as $$
declare
  v_post uuid := p_post_id;
  v_like public.likes;
  v_count integer;
begin
  if (p_post_id is null) = (p_comment_id is null) then
    raise exception 'Like either a post or a comment.' using errcode = 'PT400';
  end if;
  if p_comment_id is not null then
    select c.post_id into v_post from public.comments c where c.id = p_comment_id;
    if not found or exists (select 1 from public.comments c where c.id = p_comment_id
                            and public.blocked_between(auth.uid(), c.author_id)) then
      raise exception 'Comment not found' using errcode = 'PT404';
    end if;
  end if;
  if not public._post_visible(v_post) then
    raise exception 'Post not found' using errcode = 'PT404';
  end if;

  if p_post_id is not null then
    insert into public.likes (user_id, post_id) values (auth.uid(), p_post_id)
      on conflict (user_id, post_id) do nothing;
    select * into v_like from public.likes l where l.user_id = auth.uid() and l.post_id = p_post_id;
    select p.like_count into v_count from public.posts p where p.id = p_post_id;
  else
    insert into public.likes (user_id, comment_id) values (auth.uid(), p_comment_id)
      on conflict (user_id, comment_id) do nothing;
    select * into v_like from public.likes l where l.user_id = auth.uid() and l.comment_id = p_comment_id;
    select c.like_count into v_count from public.comments c where c.id = p_comment_id;
  end if;
  return to_jsonb(v_like) || jsonb_build_object('like_count', v_count);
end
$$;

create or replace function public.unlike(p_id uuid)
returns boolean
language sql security definer set search_path = ''
as $$
  with d as (
    delete from public.likes l
    where l.user_id = auth.uid() and (l.id = p_id or l.post_id = p_id or l.comment_id = p_id)
    returning 1)
  select exists (select 1 from d)
$$;

-- Top-level comments: relevant = most liked, then most replied, then newest;
-- recent = newest first. Empty array → 'Be the first to comment'.
create or replace function public.list_comments(p_post_id uuid, p_sort text default 'relevant')
returns jsonb
language plpgsql stable security definer set search_path = ''
as $$
declare
  v_sort text := coalesce(nullif(p_sort, ''), 'relevant');
begin
  if v_sort not in ('relevant', 'recent') then
    raise exception 'sort must be relevant or recent.' using errcode = 'PT400';
  end if;
  if not public._post_visible(p_post_id) then
    raise exception 'Post not found' using errcode = 'PT404';
  end if;
  return coalesce((
    select jsonb_agg(public._comment_json(c) order by
      case when v_sort = 'relevant' then c.like_count end desc nulls last,
      case when v_sort = 'relevant' then
        (select count(*) from public.comments r where r.parent_id = c.id) end desc nulls last,
      c.created_at desc, c.id desc)
    from public.comments c
    where c.post_id = p_post_id and c.parent_id is null
      and not public.blocked_between(auth.uid(), c.author_id)), '[]'::jsonb);
end
$$;

create or replace function public.create_comment(p_post_id uuid, p_parent_id uuid, p_body text)
returns jsonb
language plpgsql security definer set search_path = ''
as $$
declare
  v_c public.comments;
begin
  if not public._post_visible(p_post_id) then
    raise exception 'Post not found' using errcode = 'PT404';
  end if;
  if p_body is null or p_body !~ '\S' then
    raise exception 'Write a comment first.' using errcode = 'PT400';
  end if;
  insert into public.comments (post_id, author_id, parent_id, body)
  values (p_post_id, auth.uid(), p_parent_id, btrim(p_body))
  returning * into v_c;
  return public._comment_json(v_c);
end
$$;

create or replace function public.vote_poll(p_post_id uuid, p_option_index integer)
returns jsonb
language plpgsql security definer set search_path = ''
as $$
declare
  v_vote public.poll_votes;
  v_post public.posts;
begin
  if not public._post_visible(p_post_id) then
    raise exception 'Post not found' using errcode = 'PT404';
  end if;
  if p_option_index is null then
    raise exception 'Choose one of the poll options.' using errcode = 'PT400';
  end if;
  insert into public.poll_votes (post_id, user_id, option_index)
  values (p_post_id, auth.uid(), p_option_index)
  on conflict (post_id, user_id) do update set option_index = excluded.option_index
  returning * into v_vote;
  select * into v_post from public.posts p where p.id = p_post_id;
  return to_jsonb(v_vote) || jsonb_build_object(
    'my_vote', v_vote.option_index,
    'poll_results', public._post_json(v_post) -> 'poll_results');
end
$$;

create or replace function public.save_bookmark(p_post_id uuid, p_job_id uuid)
returns jsonb
language plpgsql security definer set search_path = ''
as $$
declare
  v_b public.bookmarks;
begin
  if (p_post_id is null) = (p_job_id is null) then
    raise exception 'Save either a post or a job.' using errcode = 'PT400';
  end if;
  if p_post_id is not null then
    if not public._post_visible(p_post_id) then
      raise exception 'Post not found' using errcode = 'PT404';
    end if;
    insert into public.bookmarks (user_id, post_id) values (auth.uid(), p_post_id)
      on conflict (user_id, post_id) do nothing;
    select * into v_b from public.bookmarks b where b.user_id = auth.uid() and b.post_id = p_post_id;
  else
    if not public.can_see_job(p_job_id) or exists (
         select 1 from public.jobs j where j.id = p_job_id
           and public.blocked_between(auth.uid(), j.poster_id)) then
      raise exception 'Job not found' using errcode = 'PT404';
    end if;
    insert into public.bookmarks (user_id, job_id) values (auth.uid(), p_job_id)
      on conflict (user_id, job_id) do nothing;
    select * into v_b from public.bookmarks b where b.user_id = auth.uid() and b.job_id = p_job_id;
  end if;
  return to_jsonb(v_b);
end
$$;

create or replace function public.remove_bookmark(p_id uuid)
returns boolean
language sql security definer set search_path = ''
as $$
  with d as (
    delete from public.bookmarks b
    where b.user_id = auth.uid() and (b.id = p_id or b.post_id = p_id or b.job_id = p_id)
    returning 1)
  select exists (select 1 from d)
$$;

-- Profile → Saved: bookmark rows + `post` (_post_json) or `job` (row +
-- poster card), only while the target is live and still visible.
create or replace function public.list_bookmarks()
returns jsonb
language sql stable security definer set search_path = ''
as $$
  select coalesce(jsonb_agg(x.j order by x.created_at desc, x.id desc), '[]'::jsonb)
  from (
    select b.id, b.created_at,
      to_jsonb(b) || jsonb_build_object('item_kind', 'post', 'post', public._post_json(p)) as j
    from public.bookmarks b
    join public.posts p on p.id = b.post_id
    where b.user_id = auth.uid() and public._post_visible(p.id)
    union all
    select b.id, b.created_at,
      to_jsonb(b) || jsonb_build_object('item_kind', 'job', 'job',
        to_jsonb(jb) || jsonb_build_object('poster', public._profile_card(jb.poster_id))) as j
    from public.bookmarks b
    join public.jobs jb on jb.id = b.job_id
    where b.user_id = auth.uid() and public.can_see_job(jb.id)
      and not public.blocked_between(auth.uid(), jb.poster_id)
  ) x
$$;

create or replace function public.add_content_filter(p_kind text, p_post_id uuid, p_muted_user_id uuid)
returns jsonb
language plpgsql security definer set search_path = ''
as $$
declare
  v_f public.content_filters;
begin
  if p_kind = 'hide_post' then
    if p_post_id is null or p_muted_user_id is not null then
      raise exception 'Hide needs a post_id only.' using errcode = 'PT400';
    end if;
    if not public._post_visible(p_post_id) then
      raise exception 'Post not found' using errcode = 'PT404';
    end if;
    insert into public.content_filters (user_id, kind, post_id) values (auth.uid(), 'hide_post', p_post_id)
      on conflict (user_id, post_id) do nothing;
    select * into v_f from public.content_filters f where f.user_id = auth.uid() and f.post_id = p_post_id;
  elsif p_kind = 'mute_author' then
    if p_muted_user_id is null or p_post_id is not null then
      raise exception 'Mute needs a muted_user_id only.' using errcode = 'PT400';
    end if;
    if p_muted_user_id = auth.uid() then
      raise exception 'You cannot mute yourself.' using errcode = 'PT400';
    end if;
    if not exists (select 1 from public.profiles p where p.id = p_muted_user_id) then
      raise exception 'User not found' using errcode = 'PT404';
    end if;
    insert into public.content_filters (user_id, kind, muted_user_id)
    values (auth.uid(), 'mute_author', p_muted_user_id)
      on conflict (user_id, muted_user_id) do nothing;
    select * into v_f from public.content_filters f
    where f.user_id = auth.uid() and f.muted_user_id = p_muted_user_id;
  else
    raise exception 'kind must be hide_post or mute_author.' using errcode = 'PT400';
  end if;
  return to_jsonb(v_f);
end
$$;

create or replace function public.remove_content_filter(p_id uuid)
returns boolean
language sql security definer set search_path = ''
as $$
  with d as (
    delete from public.content_filters f
    where f.user_id = auth.uid() and (f.id = p_id or f.post_id = p_id or f.muted_user_id = p_id)
    returning 1)
  select exists (select 1 from d)
$$;

revoke all on function
  public._post_visible(uuid), public._comment_json(public.comments),
  public.like_target(uuid, uuid), public.unlike(uuid),
  public.list_comments(uuid, text), public.create_comment(uuid, uuid, text),
  public.vote_poll(uuid, integer), public.save_bookmark(uuid, uuid),
  public.remove_bookmark(uuid), public.list_bookmarks(),
  public.add_content_filter(text, uuid, uuid), public.remove_content_filter(uuid)
  from public, anon, authenticated;

grant execute on function
  public.like_target(uuid, uuid), public.unlike(uuid),
  public.list_comments(uuid, text), public.create_comment(uuid, uuid, text),
  public.vote_poll(uuid, integer), public.save_bookmark(uuid, uuid),
  public.remove_bookmark(uuid), public.list_bookmarks(),
  public.add_content_filter(text, uuid, uuid), public.remove_content_filter(uuid)
  to authenticated, service_role;
grant execute on function public._post_visible(uuid), public._comment_json(public.comments)
  to service_role;
