-- format7_data.tar.gz の作り方（段階 120）: a78d94e（0.2.40、データ形式のバージョン 7）の heimdb を -no-auth -db fx7 -data <空のディレクトリ> で起動し、
-- mysql -uroot fx7 < format7_data.sql を流して止め、LOCK を除いたディレクトリの中身を tar.gz にした。
CREATE TABLE wfLocs (IP BINARY(16) NOT NULL DEFAULT '', ctime INT UNSIGNED NOT NULL, failed TINYINT UNSIGNED NOT NULL, city VARCHAR(255) DEFAULT '', lat FLOAT(10,7) DEFAULT 0, lon FLOAT(10,7) DEFAULT 0, PRIMARY KEY (IP), KEY klat (lat));
INSERT INTO wfLocs (IP, ctime, failed, city, lat, lon) VALUES ('a', 1, 0, 'Tokyo', 35.6812363, 139.7671248), ('b', 2, 0, 'Osaka', 34.6937378, 135.5021651), ('c', 3, 1, 'Null Island', 0, 0);
CREATE TABLE fd (id INT PRIMARY KEY, d DOUBLE(17,6) UNSIGNED, e DOUBLE(14,4), du DOUBLE UNSIGNED, p FLOAT, q DOUBLE, n DECIMAL(10,2), KEY kd (d), KEY ke (e), KEY kn (n));
INSERT INTO fd VALUES (1, 1700000000.1234567, 12.34567, 5, 1.5, 0.1, 1.50), (2, 1700000001.5, -0.00004, 6, 0.1, 2.5, -2.25), (3, 0, 99999.99999, 7, -3.25, 1e300, 0), (4, NULL, NULL, NULL, NULL, NULL, NULL);
CREATE TABLE sb (id INT PRIMARY KEY, s SET('a','b','c'), b BIT(8), b64 BIT(64), KEY ks (s), KEY kb (b), KEY kb64 (b64), UNIQUE KEY us (s, b));
INSERT INTO sb VALUES (1, 'a', 5, 1), (2, 'a,c', 255, 18446744073709551615), (3, '', 0, 9223372036854775808), (4, 'b,c', 16, 0), (5, NULL, NULL, NULL), (6, 'a', 6, 2);
CALL heimdb_branch_create('dev');
USE `fx7@dev`;
INSERT INTO fd VALUES (5, 1.5, 2.5, 1, 2.5, 3.5, 4.50);
UPDATE sb SET s = 'c' WHERE id = 1;
USE fx7;
DELETE FROM wfLocs WHERE ctime = 3;
