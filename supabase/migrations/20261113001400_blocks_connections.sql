-- Blocks and connections API (blockUser, unblockUser, listBlocks,
-- sendConnectionRequest, respondToConnection, deleteConnection,
-- listConnections). block_user, unblock_user, send_connection_request and
-- respond_to_connection live in the social graph migration; this adds the
-- list/delete RPCs and the Realtime broadcast of connection changes.

-- Small public card of a profile, used in block and connection lists.
create or replace function public._profile_card(p_user_id uuid)
returns jsonb
language sql stable security definer set search_path = ''
as $$
  select jsonb_build_object(
    'id', p.id, 'username', p.username, 'full_name', p.full_name,
    'headline', p.headline, 'avatar_url', p.avatar_url)
  from public.profiles p where p.id = p_user_id
$$;

-- Settings → Blocked accounts: the caller's blocks, newest first, each with
-- the blocked user's `profile` card.
create or replace function public.list_blocks()
returns jsonb
language sql stable security definer set search_path = ''
as $$
  select coalesce(jsonb_agg(
           to_jsonb(b) || jsonb_build_object('profile', public._profile_card(b.blocked_id))
           order by b.created_at desc, b.id), '[]'::jsonb)
  from public.blocks b
  where b.blocker_id = auth.uid()
$$;

-- Withdraw a request, cancel, or remove a connection: either party deletes
-- the pair row and the relationship is Idle. Nothing is notified.
-- PT404 unknown or not a party.
create or replace function public.delete_connection(p_connection_id uuid)
returns boolean
language plpgsql security definer set search_path = ''
as $$
begin
  delete from public.connections c
  where c.id = p_connection_id
    and auth.uid() in (c.requester_id, c.addressee_id);
  if not found then
    raise exception 'Request not found.' using errcode = 'PT404';
  end if;
  return true;
end;
$$;

-- The caller's connections for one tab, with counts for every tab:
--   incoming  pending requests addressed to the caller (Accept / Ignore)
--   sent      the caller's requests; a silently declined one reads 'pending'
--   connected mutual connections
-- Returns {tab, counts: {incoming, sent, connected}, items: [connection row
-- + `peer` profile card]}, newest first. Pairs with a block are excluded.
-- PT400 unknown tab.
create or replace function public.list_connections(p_tab text)
returns jsonb
language plpgsql stable security definer set search_path = ''
as $$
declare
  v_me uuid := auth.uid();
  v_items jsonb;
  v_counts jsonb;
begin
  if p_tab is null or p_tab not in ('incoming', 'sent', 'connected') then
    raise exception 'Tab must be incoming, sent or connected.' using errcode = 'PT400';
  end if;

  with mine as (
    select c.*,
           case when c.requester_id = v_me then c.addressee_id else c.requester_id end as peer_id,
           case
             when c.state = 'connected' then 'connected'
             when c.requester_id = v_me then 'sent'
             when c.state = 'pending' then 'incoming'
           end as tab
    from public.connections c
    where (c.requester_id = v_me or c.addressee_id = v_me)
      and not public.blocked_between(c.requester_id, c.addressee_id)
  )
  select
    coalesce(jsonb_agg(
      jsonb_build_object(
        'id', m.id, 'pair_key', m.pair_key, 'requester_id', m.requester_id,
        'addressee_id', m.addressee_id,
        -- silent decline: the requester never learns about it
        'state', case when m.tab = 'sent' then 'pending' else m.state end,
        'created_at', m.created_at, 'updated_at', m.updated_at,
        'peer', public._profile_card(m.peer_id))
      order by m.updated_at desc, m.id) filter (where m.tab = p_tab), '[]'::jsonb),
    jsonb_build_object(
      'incoming', count(*) filter (where m.tab = 'incoming'),
      'sent', count(*) filter (where m.tab = 'sent'),
      'connected', count(*) filter (where m.tab = 'connected'))
  into v_items, v_counts
  from mine m;

  return jsonb_build_object('tab', p_tab, 'counts', v_counts, 'items', v_items);
end;
$$;

-- Realtime: connection inserts, state changes and deletes reach both
-- parties (Realtime applies connections_select_party per subscriber), so
-- list, profile and chat update without a reload.
do $$
begin
  if not exists (select 1 from pg_publication where pubname = 'supabase_realtime') then
    create publication supabase_realtime;
  end if;
  if not exists (
    select 1 from pg_publication_tables
    where pubname = 'supabase_realtime' and schemaname = 'public' and tablename = 'connections'
  ) then
    alter publication supabase_realtime add table public.connections;
  end if;
end
$$;

revoke all on function
  public._profile_card(uuid), public.list_blocks(), public.delete_connection(uuid),
  public.list_connections(text)
  from public, anon, authenticated;

grant execute on function
  public.list_blocks(), public.delete_connection(uuid), public.list_connections(text)
  to authenticated, service_role;
grant execute on function public._profile_card(uuid) to service_role;
