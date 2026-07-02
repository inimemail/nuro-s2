UPDATE groups
SET allow_image_generation = TRUE
WHERE platform = 'grok'
  AND allow_image_generation = FALSE
  AND deleted_at IS NULL;
