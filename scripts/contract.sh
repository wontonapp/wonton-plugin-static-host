#!/usr/bin/env bash
# 对着真的容器跑 static_web 和 web_storage 的契约：scripts/contract.sh
#
# 照核心的布局建一个临时目录（static/<插件 ID>/、static/.staging/、data/<插件 ID>/），只读、读写分别挂给 Caddy 和
# 存储服务，像网关一样带上 X-Wonton-Plugin 用 curl 逐项请求。不经核心，所以网关那一侧（删头、CSP、fallback）不在这里测。
#
#   STORAGE_IMAGE  存储服务的镜像；不设时从 images/wonton-web-storage 构建成 sh-test-storage，跑完删掉。
#
# 只建、只删名字以 sh-test- 开头、带本次进程号的容器。任何一项不符就以 1 退出，并列出不符的项。
# shellcheck disable=SC2015 # 下面的 cond && ok || bad：ok 只是 printf，不会失败落到 bad。
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
command -v docker >/dev/null && command -v curl >/dev/null || { echo "需要 docker 和 curl" >&2; exit 2; }

caddy_image="$(grep -m1 -oE 'caddy:[^[:space:]]+' "$root/docker-compose.yml")"
storage_image="${STORAGE_IMAGE:-}"
static_ctr="sh-test-static-$$"
storage_ctr="sh-test-storage-$$"
work="$(mktemp -d "${TMPDIR:-/tmp}/sh-test-contract.XXXXXX")"
built=""
cleanup() {
  docker rm -f "$static_ctr" "$storage_ctr" >/dev/null 2>&1 || true
  [[ -z "$built" ]] || docker rmi "$built" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

if [[ -z "$storage_image" ]]; then
  storage_image=sh-test-storage
  docker build -q -t "$storage_image" "$root/images/wonton-web-storage" >/dev/null
  built="$storage_image"
fi

A=app.example.a
B=app.example.b
mkdir -p "$work/static/$A/sub" "$work/static/$A/empty" "$work/static/$B" "$work/static/.staging/x" "$work/data/$A" "$work/data/$B"
printf '<!doctype html><title>A</title>A-v1\n' >"$work/static/$A/index.html"
printf 'console.log(1)\n' >"$work/static/$A/app.js"
printf 'sub\n' >"$work/static/$A/sub/index.html"
printf 'only-a\n' >"$work/static/$A/a.txt"
printf 'B-SECRET\n' >"$work/static/$B/secret.txt"
printf 'STAGING-SECRET\n' >"$work/static/.staging/x/index.html"
printf 'B-DATA\n' >"$work/data/$B/secret.json"
chmod -R a+rX "$work"

user="$(id -u):$(id -g)"
docker run -d --name "$static_ctr" --user "$user" -p 127.0.0.1::8080 \
  -v "$root/Caddyfile:/etc/caddy/Caddyfile:ro" -v "$work/static:/srv/static:ro" "$caddy_image" >/dev/null
docker run -d --name "$storage_ctr" --user "$user" -p 127.0.0.1::8081 \
  -v "$work/data:/srv/data" "$storage_image" >/dev/null
static="http://$(docker port "$static_ctr" 8080/tcp | head -1)"
storage="http://$(docker port "$storage_ctr" 8081/tcp | head -1)"

failures=0
body="$work/body"
headers="$work/headers"
# req <URL 前缀> <插件 ID 或 -> <curl 参数…>：设置 $code、$body、$headers。
req() {
  local base="$1" id="$2"
  shift 2
  local hdr=()
  [[ "$id" == - ]] || hdr=(-H "X-Wonton-Plugin: $id")
  code="$(curl -s --noproxy '*' --path-as-is -o "$body" -D "$headers" -w '%{http_code}' "${hdr[@]}" "$@" "$base")" || code=000
}
ok() { printf 'PASS %s\n' "$1"; }
bad() { printf 'FAIL %s：%s\n' "$1" "$2"; failures=$((failures + 1)); }
# expect <名字> <期望状态码（可写成 400|404）> <URL 前缀> <插件 ID 或 -> <curl 参数…>
expect() {
  local name="$1" want="$2"
  shift 2
  req "$@"
  if [[ "$code" =~ ^($want)$ ]]; then ok "$name（$code）"; else bad "$name" "状态 $code，期望 $want：$(head -c 160 "$body")"; fi
}
has_body() { grep -qF -- "$2" "$body" && ok "$1" || bad "$1" "响应体没有 $2：$(head -c 160 "$body")"; }
lacks_body() { ! grep -qF -- "$2" "$body" && ok "$1" || bad "$1" "响应体里有 $2"; }
header() { tr -d '\r' <"$headers" | awk -v k="$(tr '[:upper:]' '[:lower:]' <<<"$1")" -F': ' 'tolower($1)==k {print $2}' | tail -1; }
has_header() {
  local v
  v="$(header "$2")"
  [[ "$v" == *"$3"* ]] && ok "$1（$2: $v）" || bad "$1" "$2 是 '$v'，期望含 '$3'"
}

# docker run 没有 compose 的 healthcheck，这里按 compose 里写的命令在容器里执行一次。
health_once() {
  local name="$1"
  shift
  docker exec "$@" >/dev/null 2>&1 && ok "$name" || bad "$name" "健康检查命令失败"
}
for _ in $(seq 30); do curl -s --noproxy '*' -o /dev/null "$static" && curl -s --noproxy '*' -o /dev/null "$storage" && break; sleep 1; done
health_once "static 健康检查" "$static_ctr" sh -c 'test -d /srv/static && wget -q -O /dev/null http://127.0.0.1:8079/healthz'
health_once "storage 健康检查" "$storage_ctr" /usr/local/bin/wonton-web-storage -healthcheck
echo "== 空闲内存"
docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$static_ctr" "$storage_ctr"

echo "== static_web"
expect "没有头" 400 "$static/" -
for v in '../x' 'App.example.a' 'a' '.staging' "a.$(printf 'b%.0s' $(seq 62))" "$A, ../$B" "$A,$B" "$A/../$B"; do
  expect "非法头 $v" 400 "$static/" "$v"
done
expect "首页" 200 "$static/" "$A"
has_body "首页内容" "A-v1"
has_header "首页类型" content-type text/html
etag="$(header etag)"
[[ -n "$etag" ]] && ok "首页有 ETag" || bad "首页有 ETag" "没有 ETag"
expect "If-None-Match" 304 "$static/" "$A" -H "If-None-Match: $etag"
expect "Range" 206 "$static/" "$A" -H "Range: bytes=0-8"
[[ "$(cat "$body")" == '<!doctype' ]] && ok "Range 内容" || bad "Range 内容" "$(cat "$body")"
expect "HEAD" 200 "$static/" "$A" -I
lacks_body "HEAD 没有响应体" "A-v1"
expect "脚本" 200 "$static/app.js" "$A"
has_header "脚本类型" content-type javascript
expect "子目录首页" 200 "$static/sub/" "$A"
has_body "子目录内容" "sub"
expect "缺文件" 404 "$static/nope.html" "$A"
expect "没有 index.html 的目录不列目录" 404 "$static/empty/" "$A"
expect "没有这个插件的子目录" 404 "$static/" app.example.missing
expect "B 读不到 A 的文件" 404 "$static/a.txt" "$B"
for path in "/../$B/secret.txt" "/%2e%2e/$B/secret.txt" "/..%2f$B%2fsecret.txt" "/%2e%2e%2f$B%2fsecret.txt" \
  "/sub/../../$B/secret.txt" "/../.staging/x/index.html" "/%2e%2e%2f.staging%2fx%2findex.html"; do
  expect "穿越 $path" '400|404' "$static$path" "$A"
  lacks_body "穿越 $path 没拿到别人的内容" SECRET
done
for m in POST PUT DELETE PATCH; do expect "$m" 405 "$static/" "$A" -X "$m"; done

# 原子替换：照核心的做法，新版本先放进 .staging/，再和插件目录对换。没有 mv --exchange 时用两次 rename。
mkdir -p "$work/static/.staging/$A-new"
printf '<!doctype html><title>A</title>A-v2-longer\n' >"$work/static/.staging/$A-new/index.html"
chmod -R a+rX "$work/static/.staging"
started="$(docker inspect -f '{{.State.StartedAt}}' "$static_ctr")"
if mv --exchange -T "$work/static/.staging/$A-new" "$work/static/$A" 2>/dev/null; then
  swap="mv --exchange"
else
  swap="两次 rename"
  mv "$work/static/$A" "$work/static/.staging/$A-old"
  mv "$work/static/.staging/$A-new" "$work/static/$A"
fi
expect "替换后（$swap）" 200 "$static/" "$A"
has_body "替换后是新内容" "A-v2-longer"
[[ "$(header etag)" != "$etag" ]] && ok "替换后 ETag 变了" || bad "替换后 ETag 变了" "还是 $etag"
expect "替换后旧 ETag 不再 304" 200 "$static/" "$A" -H "If-None-Match: $etag"
[[ "$(docker inspect -f '{{.State.StartedAt}}' "$static_ctr")" == "$started" ]] && ok "替换没有重启 Caddy" ||
  bad "替换没有重启 Caddy" "容器重启过"

echo "== web_storage"
expect "没有头" 400 "$storage/x.json" -
expect "非法头" 400 "$storage/x.json" "$A, ../$B"
expect "没有这个插件的目录" 404 "$storage/x.json" app.example.missing
[[ ! -e "$work/data/app.example.missing" ]] && ok "不替核心建插件目录" || bad "不替核心建插件目录" "建了"
expect "PUT 新建" 201 "$storage/todo.json" "$A" -X PUT --data-binary '{"v":1}' -H 'Content-Type: application/json'
tag1="$(header etag)"
expect "GET" 200 "$storage/todo.json" "$A"
[[ "$(cat "$body")" == '{"v":1}' ]] && ok "GET 字节相同" || bad "GET 字节相同" "$(cat "$body")"
has_header "GET 类型" content-type application/json
[[ "$(header etag)" == "$tag1" && -n "$tag1" ]] && ok "GET 的 ETag 和 PUT 的一样" || bad "GET 的 ETag" "$(header etag) / $tag1"
expect "HEAD" 200 "$storage/todo.json" "$A" -I
expect "PUT 覆盖" 204 "$storage/todo.json" "$A" -X PUT --data-binary '{"v":2}'
tag2="$(header etag)"
[[ -n "$tag2" && "$tag2" != "$tag1" ]] && ok "覆盖后 ETag 变了" || bad "覆盖后 ETag 变了" "$tag1 → $tag2"
expect "If-Match 不符" 412 "$storage/todo.json" "$A" -X PUT --data-binary x -H "If-Match: $tag1"
expect "If-None-Match: * 而文件在" 412 "$storage/todo.json" "$A" -X PUT --data-binary x -H 'If-None-Match: *'
expect "If-Match 相符" 204 "$storage/todo.json" "$A" -X PUT --data-binary '{"v":3}' -H "If-Match: $tag2"
expect "If-None-Match: * 而文件不在" 201 "$storage/fresh.json" "$A" -X PUT --data-binary 1 -H 'If-None-Match: *'
expect "父目录不在" 409 "$storage/missing/x.json" "$A" -X PUT --data-binary 1
expect "Content-Range" 501 "$storage/todo.json" "$A" -X PUT --data-binary 1 -H 'Content-Range: bytes 0-0/1'
expect "MKCOL" 201 "$storage/notes" "$A" -X MKCOL
expect "MKCOL 已在" 405 "$storage/notes" "$A" -X MKCOL
expect "MKCOL 父目录不在" 409 "$storage/a/b" "$A" -X MKCOL
expect "MKCOL 带请求体" 415 "$storage/other" "$A" -X MKCOL --data-binary '<x/>'
expect "PUT 进子目录" 201 "$storage/notes/n1.json" "$A" -X PUT --data-binary 1
expect "PROPFIND 没写 Depth" 501 "$storage/" "$A" -X PROPFIND
expect "PROPFIND Depth: infinity" 501 "$storage/" "$A" -X PROPFIND -H 'Depth: infinity'
expect "PROPFIND Depth: 1" 207 "$storage/" "$A" -X PROPFIND -H 'Depth: 1' --data-binary '<propfind xmlns="DAV:"><allprop/></propfind>'
for want in '<D:href>todo.json</D:href>' '<D:href>notes/</D:href>' '<D:getcontentlength>' '<D:getetag>' '<D:getlastmodified>' '<D:collection>'; do
  has_body "PROPFIND 有 $want" "$want"
done
lacks_body "PROPFIND 不列 B 的文件" secret.json
expect "PROPFIND Depth: 0" 207 "$storage/notes/n1.json" "$A" -X PROPFIND -H 'Depth: 0'
has_body "Depth: 0 只有自己" '<D:href>n1.json</D:href>'
expect "DELETE 非空目录" 409 "$storage/notes" "$A" -X DELETE
expect "DELETE 文件" 204 "$storage/notes/n1.json" "$A" -X DELETE
expect "DELETE 以后 404" 404 "$storage/notes/n1.json" "$A"
expect "DELETE 不在的" 404 "$storage/notes/n1.json" "$A" -X DELETE
expect "DELETE 空目录" 204 "$storage/notes" "$A" -X DELETE
for m in LOCK UNLOCK MOVE COPY PROPPATCH; do expect "$m" 405 "$storage/todo.json" "$A" -X "$m"; done
for m in POST OPTIONS PATCH; do expect "$m" 501 "$storage/todo.json" "$A" -X "$m"; done
expect "A 读不到 B 的文件" 404 "$storage/secret.json" "$A"
for path in "/../$B/secret.json" "/%2e%2e/$B/secret.json" "/..%2f$B%2fsecret.json" "/x/../../$B/secret.json" "/.wonton-put-x"; do
  expect "穿越 $path" '400|404' "$storage$path" "$A"
  lacks_body "穿越 $path 没拿到别人的内容" B-DATA
done
expect "穿越写" '400|404' "$storage/..%2f$B%2fsecret.json" "$A" -X PUT --data-binary pwned
[[ "$(cat "$work/data/$B/secret.json")" == B-DATA ]] && ok "B 的文件没被改" || bad "B 的文件没被改" "被改了"

# 上限：超了回 507，原文件不动；不带 Content-Length（分块）时边收边查。
head -c $((8 * 1024 * 1024 + 1)) /dev/zero >"$work/big"
expect "单文件超 8 MiB" 507 "$storage/todo.json" "$A" -X PUT --data-binary "@$work/big"
expect "单文件超 8 MiB（分块）" 507 "$storage/todo.json" "$A" -X PUT -H 'Transfer-Encoding: chunked' --data-binary "@$work/big"
expect "超限后原文件" 200 "$storage/todo.json" "$A"
[[ "$(cat "$body")" == '{"v":3}' ]] && ok "超限后原文件不动" || bad "超限后原文件不动" "$(head -c 80 "$body")"
head -c $((8 * 1024 * 1024)) /dev/zero >"$work/full"
for i in 1 2 3 4 5 6 7; do req "$storage/f$i.bin" "$A" -X PUT --data-binary "@$work/full"; done
expect "总量超 64 MiB" 507 "$storage/f8.bin" "$A" -X PUT --data-binary "@$work/full"
stray="$(find "$work/data/$A" -name '.wonton-put-*' | wc -l | tr -d ' ')"
[[ "$stray" == 0 ]] && ok "没留下临时文件" || bad "没留下临时文件" "$stray 个"
owner="$(stat -c '%u:%g' "$work/data/$A/todo.json" 2>/dev/null || stat -f '%u:%g' "$work/data/$A/todo.json")"
[[ "$owner" == "$user" ]] && ok "写出的文件属主是 $user" || bad "写出的文件属主" "$owner"

if ((failures)); then
  echo "$failures 项不符" >&2
  exit 1
fi
echo "全部符合"
