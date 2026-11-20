-- format6_data.tar.gz の作り方（段階 117）: 3d02beb（データ形式のバージョン 6）の heimdb を -no-auth -db fx6 -data <空のディレクトリ> で起動し、
-- mysql -uroot fx6 < format6_data.sql を流して止め、LOCK を除いたディレクトリの中身を tar.gz にした。
CREATE TABLE wp_wc_product_meta_lookup (product_id BIGINT NOT NULL, sku VARCHAR(100) NULL DEFAULT '', `virtual` TINYINT(1) NULL DEFAULT 0, min_price DECIMAL(19,4) NULL DEFAULT NULL, max_price DECIMAL(19,4) NULL DEFAULT NULL, onsale TINYINT(1) NULL DEFAULT 0, stock_quantity DOUBLE NULL DEFAULT NULL, stock_status VARCHAR(100) NULL DEFAULT 'instock', PRIMARY KEY (product_id), KEY `virtual` (`virtual`), KEY onsale (onsale), KEY stock_status (stock_status), KEY stock_quantity (stock_quantity), KEY min_max_price (min_price, max_price), KEY sku (sku(50)));
INSERT INTO wp_wc_product_meta_lookup (product_id, sku, min_price, max_price, stock_quantity) VALUES (1, 'a', 10.0000, 12.5000, 5), (2, 'b', 9.9950, 9.9950, NULL), (3, 'c', 0, 0, 0), (4, 'd', -1.5000, 2.0000, -2.5), (5, 'e', NULL, NULL, 1e300), (6, 'f', 10.0000, 10.0000, 5);
CREATE TABLE ft (id INT PRIMARY KEY, f FLOAT, e ENUM('z','b','a'), KEY kf (f), KEY ke (e), KEY kfe (f, e));
INSERT INTO ft VALUES (1, 0.1, 'z'), (2, 0, 'b'), (3, -0.0, 'a'), (4, 1.5, NULL), (5, NULL, 'z'), (6, -3.25, 'a');
CREATE TABLE sp (code VARCHAR(20) PRIMARY KEY, amount DECIMAL(10,2) NOT NULL, UNIQUE KEY ua (amount));
INSERT INTO sp VALUES ('x', 1.50), ('y', -2.25), ('z', 0);
CALL heimdb_branch_create('dev');
USE `fx6@dev`;
UPDATE wp_wc_product_meta_lookup SET min_price = 7.2500 WHERE product_id = 6;
INSERT INTO ft VALUES (7, 2.5, 'b');
USE fx6;
DELETE FROM ft WHERE id = 1;
