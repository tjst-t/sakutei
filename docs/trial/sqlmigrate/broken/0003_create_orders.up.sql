-- 壊れた 0003（手順 3・4）。2 文目で表の名前を書き間違えていて（users → user_list）失敗する。
-- 1 文目の CREATE TABLE はトランザクションごと巻き戻る（orders は残らない）。
CREATE TABLE orders (id INTEGER PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id), total INTEGER NOT NULL);
INSERT INTO orders (user_id, total) SELECT id, 0 FROM user_list;
