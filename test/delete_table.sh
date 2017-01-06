#!/bin/bash

SQL_FILE=/tmp/create_table.sql.$$

cat << EOF > $SQL_FILE

DROP TABLE IF EXISTS table_with_all_types;

EOF

cd $VTTOP/examples/local

./lvtctl.sh ApplySchema -sql "$(cat $SQL_FILE)" test_keyspace

rm $SQL_FILE

