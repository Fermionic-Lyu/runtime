BEGIN;
INSERT INTO envs (id, updated_at, public, team_id)
VALUES ('instapoc', now(), false, '0b8a3ded-4489-4722-afd1-1d82e64ec2d5')
ON CONFLICT (id) DO NOTHING;

INSERT INTO env_builds
  (id, updated_at, finished_at, status, vcpu, ram_mb, free_disk_size_mb,
   total_disk_size_mb, kernel_version, firecracker_version, envd_version, env_id)
VALUES
  ('81f7e550-cd31-4f7f-80e5-f6a58ee57e00', now(), now(), 'success', 1, 512, 0,
   0, 'external-runtime', 'v0.0.0', '0.2.0', 'instapoc')
ON CONFLICT (id) DO NOTHING;

INSERT INTO env_build_assignments (env_id, build_id, tag)
SELECT 'instapoc', '81f7e550-cd31-4f7f-80e5-f6a58ee57e00', 'default'
WHERE NOT EXISTS (
  SELECT 1 FROM env_build_assignments
  WHERE env_id='instapoc' AND build_id='81f7e550-cd31-4f7f-80e5-f6a58ee57e00' AND tag='default'
);
COMMIT;
