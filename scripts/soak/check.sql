-- 長時間の負荷試験の数え直しの検査（docs/bench.md の「段階 108」）。WordPress が持つ数と、数え直した数が違う行を出す。
-- 試験の前にも流して、元のデータの違い（あれば）を基準にする。
SELECT 'comment_count', p.ID, p.comment_count, COALESCE(c.n, 0)
  FROM wp_posts p LEFT JOIN (SELECT comment_post_ID, COUNT(*) AS n FROM wp_comments WHERE comment_approved = '1' GROUP BY comment_post_ID) c
    ON c.comment_post_ID = p.ID
  WHERE p.post_type IN ('post', 'page') AND p.comment_count <> COALESCE(c.n, 0);
SELECT 'term_count', tt.term_taxonomy_id, tt.count, COALESCE(r.n, 0)
  FROM wp_term_taxonomy tt LEFT JOIN (SELECT tr.term_taxonomy_id, COUNT(*) AS n FROM wp_term_relationships tr JOIN wp_posts p ON p.ID = tr.object_id
      WHERE p.post_type = 'post' AND p.post_status = 'publish' GROUP BY tr.term_taxonomy_id) r ON r.term_taxonomy_id = tt.term_taxonomy_id
  WHERE tt.taxonomy IN ('category', 'post_tag') AND tt.count <> COALESCE(r.n, 0);
SELECT 'orphan_postmeta', COUNT(*) FROM wp_postmeta m LEFT JOIN wp_posts p ON p.ID = m.post_id WHERE p.ID IS NULL;
SELECT 'orphan_commentmeta', COUNT(*) FROM wp_commentmeta m LEFT JOIN wp_comments c ON c.comment_ID = m.comment_id WHERE c.comment_ID IS NULL;
SELECT 'orphan_comments', COUNT(*) FROM wp_comments c LEFT JOIN wp_posts p ON p.ID = c.comment_post_ID WHERE p.ID IS NULL;
SELECT 'orphan_relationships', COUNT(*) FROM wp_term_relationships tr LEFT JOIN wp_posts p ON p.ID = tr.object_id WHERE p.ID IS NULL;
