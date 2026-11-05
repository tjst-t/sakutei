-- 遅いマイグレーション（試用のため、数秒かかる計算をわざと入れてある）。
-- 実行中にデーモンの周を何周か跨がせて、1-outstanding（二重に起動しない）と、
-- 実行中にデーモンを殺したとき（手順 5）を確かめる。
ALTER TABLE users ADD COLUMN email TEXT;
CREATE TABLE user_stats AS
  WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c WHERE x < 25000000)
  SELECT count(*) AS n FROM c;
