-- Storage buckets for POST /uploads (signed upload URLs).
--
-- The API checks purpose, MIME type and size before signing
-- (internal/uploads/policy.go); each bucket enforces the largest cap and the
-- union of MIME types of the purposes that write to it, so an upload that
-- skips the API check is still bounded. Keep this file and the policy in
-- step (TestBucketMigrationMatchesPolicy fails otherwise). The project-wide
-- upload limit (supabase/config.toml [storage] file_size_limit) must be at
-- least the largest bucket cap, 50 MiB.
--
--   avatars          avatar                         3 MB  images      public
--   community-media  logo, cover                    5 MB  images      public
--   post-media       post_image 5, post_video 50,  50 MB  images,     private
--                    document 25                          video, docs
--   chat-media       chat                          25 MB  media, docs private
--                                                         voice, vCard
--   resumes          resume                        10 MB  PDF, DOCX   private
--
-- Objects are written only through signed upload URLs (no client insert
-- policy on storage.objects); object names are <user id>/<purpose>/<random>.<ext>.
-- On plain Postgres (tests) the storage schema does not exist: no-op.

do $$
begin
  if to_regclass('storage.buckets') is null then
    return;
  end if;

  insert into storage.buckets (id, name, public, file_size_limit, allowed_mime_types)
  values
    ('avatars', 'avatars', true, 3145728,
      array['image/heic', 'image/heif', 'image/jpeg', 'image/png', 'image/webp']),
    ('community-media', 'community-media', true, 5242880,
      array['image/gif', 'image/heic', 'image/heif', 'image/jpeg', 'image/png', 'image/webp']),
    ('post-media', 'post-media', false, 52428800,
      array['application/msword', 'application/pdf', 'application/vnd.ms-excel',
            'application/vnd.ms-powerpoint',
            'application/vnd.openxmlformats-officedocument.presentationml.presentation',
            'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet',
            'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
            'image/gif', 'image/heic', 'image/heif', 'image/jpeg', 'image/png', 'image/webp',
            'text/csv', 'text/plain',
            'video/3gpp', 'video/mp4', 'video/quicktime', 'video/webm']),
    ('chat-media', 'chat-media', false, 26214400,
      array['application/msword', 'application/pdf', 'application/vnd.ms-excel',
            'application/vnd.ms-powerpoint',
            'application/vnd.openxmlformats-officedocument.presentationml.presentation',
            'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet',
            'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
            'audio/3gpp', 'audio/aac', 'audio/amr', 'audio/mp4', 'audio/mpeg', 'audio/ogg',
            'audio/wav', 'audio/webm', 'audio/x-m4a', 'audio/x-wav',
            'image/gif', 'image/heic', 'image/heif', 'image/jpeg', 'image/png', 'image/webp',
            'text/csv', 'text/plain', 'text/vcard', 'text/x-vcard',
            'video/3gpp', 'video/mp4', 'video/quicktime', 'video/webm']),
    ('resumes', 'resumes', false, 10485760,
      array['application/pdf',
            'application/vnd.openxmlformats-officedocument.wordprocessingml.document'])
  on conflict (id) do update
    set public = excluded.public,
        file_size_limit = excluded.file_size_limit,
        allowed_mime_types = excluded.allowed_mime_types;
end
$$;
