import MySQLdb
import time

db = MySQLdb.connect(host='127.0.0.1', user='root', port=15891)
db.select_db('test_keyspace')
print db.ping()
print db.stat()
print db.autocommit(False)
c = db.cursor()
c.execute('select max(page) from messages')
max_page = c.fetchone()[0]
ts = int(time.time()) * 10 ** 9
db.begin()
c.execute("insert into messages values ('%s','%s', 'insert from python')" % (max_page + 3, ts))
db.commit()
print db.affected_rows()

c.execute('select count(*) from table_with_all_types')
max_id = c.fetchone()[0]

col_boolean = 1
col_byte = 64
col_short = 999
col_int = 99999999
col_long = 10 ** 12
col_float = 123456.123456789
col_double = 10 ** 16
col_bigdecimal = 1234.5678
col_string = "Vitess is cats meow!"
col_date = time.strftime('%Y-%m-%d %H:%M:%S')
col_time = time.strftime('%Y-%m-%d %H:%M:%S')
col_timestamp = time.strftime('%Y-%m-%d %H:%M:%S')
col_strtype = 'Hey ho neighbourino'

sql = "INSERT INTO table_with_all_types VALUES ('%s', '%s', '%s', '%s', '%s', '%s', '%s', '%s', '%s', '%s', '%s', '%s', '%s', '%s', '%s', '%s', '%s', '%s', '%s', '%s')"
col_int = max_id + 1
sql = sql % (col_boolean, col_byte, col_short, col_int, col_long, col_float, col_double, col_bigdecimal, col_string, col_date, col_time, col_timestamp, col_strtype, col_strtype, col_strtype, col_strtype, col_strtype, col_strtype, col_strtype, col_strtype)

db.begin()
c.execute(sql)
db.commit()
