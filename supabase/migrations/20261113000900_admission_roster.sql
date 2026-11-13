-- Admission, roster and role management: role change, member removal,
-- leave/withdraw and the admin roster listing. request_join,
-- review_join_request and transfer_ownership live in the identity migration.

-- Sets a joined member's role to admin, moderator or member. Only the
-- community's joined Owner may do this; ownership moves only by transfer.
create or replace function public.set_member_role(p_community_id uuid, p_user_id uuid, p_role text)
returns public.memberships
language plpgsql security definer set search_path = ''
as $$
declare
  v_row public.memberships;
begin
  if p_role is null or p_role not in ('admin', 'moderator', 'member') then
    raise exception 'Role must be admin, moderator or member. Ownership moves only by transfer.'
      using errcode = 'PT400';
  end if;
  if not public.has_community_role(p_community_id, array['owner']) then
    raise exception 'Only the owner can change roles.' using errcode = 'PT403';
  end if;

  select * into v_row from public.memberships
  where community_id = p_community_id and user_id = p_user_id and status = 'joined'
  for update;
  if not found then
    raise exception 'Member not found.' using errcode = 'PT404';
  end if;
  if v_row.role = 'owner' then
    raise exception 'Ownership moves only by transfer.' using errcode = 'PT403';
  end if;

  update public.memberships set role = p_role where id = v_row.id returning * into v_row;
  return v_row;
end
$$;

-- Removes a member (or a pending request) from the community. Joined owners
-- and admins only; never self (use leave) and never the Owner. Only the
-- Owner may remove an admin, since only the Owner revokes the admin role.
create or replace function public.remove_member(p_community_id uuid, p_user_id uuid)
returns void
language plpgsql security definer set search_path = ''
as $$
declare
  v_uid uuid := auth.uid();
  v_role text := public.community_role(p_community_id);
  v_row public.memberships;
begin
  if v_role is null or v_role not in ('owner', 'admin') then
    raise exception 'Admins only.' using errcode = 'PT403';
  end if;
  if p_user_id = v_uid then
    raise exception 'You cannot remove yourself. Leave the community instead.' using errcode = 'PT403';
  end if;

  select * into v_row from public.memberships
  where community_id = p_community_id and user_id = p_user_id
  for update;
  if not found then
    raise exception 'Member not found.' using errcode = 'PT404';
  end if;
  if v_row.role = 'owner' then
    raise exception 'The owner cannot be removed.' using errcode = 'PT403';
  end if;
  if v_row.role = 'admin' and v_role <> 'owner' then
    raise exception 'Only the owner can remove an admin.' using errcode = 'PT403';
  end if;

  delete from public.memberships where id = v_row.id;
end
$$;

-- Leaves a community or withdraws a pending request. The Owner must
-- transfer ownership first (PT409).
create or replace function public.leave_community(p_community_id uuid)
returns void
language plpgsql security definer set search_path = ''
as $$
declare
  v_row public.memberships;
begin
  select * into v_row from public.memberships
  where community_id = p_community_id and user_id = auth.uid()
  for update;
  if not found then
    raise exception 'You are not a member of this community.' using errcode = 'PT404';
  end if;
  if v_row.role = 'owner' then
    raise exception 'Transfer ownership before leaving.' using errcode = 'PT409';
  end if;
  delete from public.memberships where id = v_row.id;
end
$$;

-- The admin panel roster: joined members with role labels, or pending
-- requests with their answers. Joined Owners and Admins only.
create or replace function public.list_members(p_community_id uuid, p_status text default 'joined')
returns jsonb
language plpgsql stable security definer set search_path = ''
as $$
begin
  if p_status is null or p_status not in ('joined', 'pending') then
    raise exception 'status must be joined or pending' using errcode = 'PT400';
  end if;
  if not public.has_community_role(p_community_id, array['owner', 'admin']) then
    raise exception 'Admins only' using errcode = 'PT403';
  end if;

  return coalesce((
    select jsonb_agg(
      to_jsonb(m) || jsonb_build_object(
        'role_label', initcap(m.role),
        'profile', jsonb_build_object(
          'id', p.id, 'username', p.username, 'full_name', p.full_name,
          'headline', p.headline, 'avatar_url', p.avatar_url),
        'answers', coalesce((
          select jsonb_agg(jsonb_build_object(
            'id', a.id, 'question_id', q.id, 'label', q.label, 'kind', q.kind,
            'required', q.required, 'value', a.value) order by q.position, q.id)
          from public.join_answers a
          join public.community_questions q on q.id = a.question_id
          where a.membership_id = m.id), '[]'::jsonb))
      order by
        case m.role when 'owner' then 0 when 'admin' then 1 when 'moderator' then 2 else 3 end,
        case when m.status = 'pending' then m.created_at end,
        lower(p.full_name), m.id)
    from public.memberships m
    join public.profiles p on p.id = m.user_id
    where m.community_id = p_community_id and m.status = p_status
  ), '[]'::jsonb);
end
$$;

revoke all on function
  public.set_member_role(uuid, uuid, text), public.remove_member(uuid, uuid),
  public.leave_community(uuid), public.list_members(uuid, text)
  from public, anon, authenticated;
grant execute on function
  public.set_member_role(uuid, uuid, text), public.remove_member(uuid, uuid),
  public.leave_community(uuid), public.list_members(uuid, text)
  to authenticated, service_role;
