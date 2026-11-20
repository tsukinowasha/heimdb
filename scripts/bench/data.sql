-- generate.sh の後に、本物の MySQL のデータベース wpbench で実行する。日付をずらし、カテゴリとタグ、投稿メタ、
-- コメントを足す。乱数は使わず、ID から決める（何度作っても同じデータになる）。
SET SESSION cte_max_recursion_depth = 100000;

-- 記事と固定ページの日付を 1 時間ずつずらす（すべて同じ日時だと同順位の並びの差が出るので）。
UPDATE wp_posts SET post_date = '2024-01-01 00:00:00' - INTERVAL ID HOUR, post_date_gmt = '2024-01-01 00:00:00' - INTERVAL ID HOUR,
  post_modified = '2024-01-01 00:00:00' - INTERVAL ID HOUR, post_modified_gmt = '2024-01-01 00:00:00' - INTERVAL ID HOUR
  WHERE post_type IN ('post', 'page');

-- 検索の語は 1% の記事にだけ入れる。
UPDATE wp_posts SET post_content = CONCAT(post_content, ' heimdbneedle') WHERE post_type = 'post' AND ID % 100 = 7;

-- 各記事にカテゴリ 1 つとタグ 3 つ。
INSERT INTO wp_term_relationships (object_id, term_taxonomy_id, term_order)
SELECT p.ID, c.term_taxonomy_id, 0 FROM wp_posts p
JOIN (SELECT term_taxonomy_id, ROW_NUMBER() OVER (ORDER BY term_taxonomy_id) - 1 AS n FROM wp_term_taxonomy WHERE taxonomy = 'category') c
  ON c.n = p.ID % 101
WHERE p.post_type = 'post'
ON DUPLICATE KEY UPDATE term_order = 0;
INSERT IGNORE INTO wp_term_relationships (object_id, term_taxonomy_id, term_order)
SELECT p.ID, t.term_taxonomy_id, 0 FROM wp_posts p
JOIN (SELECT term_taxonomy_id, ROW_NUMBER() OVER (ORDER BY term_taxonomy_id) - 1 AS n FROM wp_term_taxonomy WHERE taxonomy = 'post_tag') t
  ON t.n IN (p.ID % 200, (p.ID * 7) % 200, (p.ID * 13) % 200)
WHERE p.post_type = 'post';

-- 各記事に投稿メタ 3 つ。
INSERT INTO wp_postmeta (post_id, meta_key, meta_value)
SELECT ID, k.meta_key, CONCAT(k.meta_key, '-', ID) FROM wp_posts
JOIN (SELECT 'bench_views' AS meta_key UNION ALL SELECT 'bench_color' UNION ALL SELECT 'bench_note') k
WHERE post_type = 'post';

-- コメント 20,000 件。最も新しい記事に 60 件、残りを記事に 4 件ずつ（ID の順に）。
INSERT INTO wp_comments (comment_post_ID, comment_author, comment_author_email, comment_author_url, comment_author_IP,
  comment_date, comment_date_gmt, comment_content, comment_karma, comment_approved, comment_agent, comment_type, comment_parent, user_id)
WITH RECURSIVE seq (n) AS (SELECT 0 UNION ALL SELECT n + 1 FROM seq WHERE n < 19999),
posts AS (SELECT ID, ROW_NUMBER() OVER (ORDER BY ID) - 1 AS k FROM wp_posts WHERE post_type = 'post'),
newest AS (SELECT MAX(ID) AS ID FROM wp_posts WHERE post_type = 'post')
SELECT IF(s.n < 60, (SELECT ID FROM newest), (SELECT ID FROM posts WHERE k = (s.n - 60) DIV 4 % 5001)),
  CONCAT('Commenter ', s.n), CONCAT('c', s.n, '@example.com'), '', '127.0.0.1',
  '2024-02-01 00:00:00' - INTERVAL s.n MINUTE, '2024-02-01 00:00:00' - INTERVAL s.n MINUTE,
  CONCAT('Comment ', s.n, ' lorem ipsum dolor sit amet.'), 0, '1', 'bench', 'comment', 0, 0
FROM seq s;

UPDATE wp_posts p SET comment_count = (SELECT COUNT(*) FROM wp_comments c WHERE c.comment_post_ID = p.ID AND c.comment_approved = '1');
