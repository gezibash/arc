# ARC adds two managed UTC timestamps. Validate them against the independent
# system clock, then return the authored document without those two fields.
export ARC_JOURNAL_BEGIN="$(date +%s)"
journal_doc() {
  python3 -c '
import datetime, os, re, sys, time
text=sys.stdin.read()
values={key: re.search(r"(?m)^"+key+r": (.+)$",text).group(1) for key in ["created_at","updated_at"]}
a,b=[datetime.datetime.fromisoformat(values[key].replace("Z","+00:00")).timestamp() for key in ["created_at","updated_at"]]
assert int(os.environ["ARC_JOURNAL_BEGIN"])-1 <= a <= b <= time.time()+1, values
sys.stdout.write(re.sub(r"(?m)^(created_at|updated_at): .+\n", "", text))
'
}
