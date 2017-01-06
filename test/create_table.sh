#!/bin/bash

SQL_FILE=/tmp/create_table.sql.$$

cat << EOF > $SQL_FILE

CREATE TABLE IF NOT EXISTS table_with_all_types (
col_boolean       BOOL,                 -- boolean
col_byte          TINYINT,              -- byte
col_short         SMALLINT,             -- short
col_int           INTEGER,              -- int
col_long          BIGINT,               -- long
col_float         FLOAT,                -- float
col_double        DOUBLE PRECISION,     -- double
col_bigdecimal    DECIMAL(13,0),        -- BigDecimal
col_string        VARCHAR(254),         -- String
col_date          DATE,                 -- Date
col_time          TIME,                 -- Time
col_timestamp     TIMESTAMP,            -- Timestamp
col_asciistream1  TINYTEXT,             -- Clob ( 2^8 bytes)
col_asciistream2  TEXT,                 -- Clob ( 2^16 bytes)
col_asciistream3  MEDIUMTEXT,           -- Clob (2^24 bytes)
col_asciistream4  LONGTEXT,             -- Clob ( 2^32 h)
col_blob1         TINYBLOB,             -- Blob ( 2^8 bytes)
col_blob2         BLOB,                 -- Blob ( 2^16 bytes)
col_blob3         MEDIUMBLOB,           -- Blob ( 2^24 bytes)
col_blob4         LONGBLOB,             -- Blob ( 2^32 bytes)
PRIMARY KEY (col_int)
)

EOF

cd $VTTOP/examples/local

./lvtctl.sh ApplySchema -sql "$(cat $SQL_FILE)" test_keyspace

rm $SQL_FILE

