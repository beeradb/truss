#!/usr/bin/env bash
# Put the truss dashboards into their own folder in a Grafana you already run,
# over its HTTP API -- for the Grafana on the FPL-Armband cluster, which this
# machine cannot reach with kubectl but can reach over HTTPS.
#
# It creates (or reuses) a folder named "Truss" and uploads every metrics
# dashboard in ../dashboards into it, bound to that Grafana's existing
# Prometheus. It stands up nothing: no datasource, no Grafana, no Prometheus.
#
#   GRAFANA_URL    the base URL, no trailing slash (e.g. https://grafana.example)
#   GRAFANA_TOKEN  a Grafana API token. ⚠️ IT MUST BE ADMIN, NOT EDITOR, and
#                  this is not obvious: under Grafana 11 RBAC an Editor token
#                  CAN create a folder but is granted no permission ON it
#                  (canSave:false), so the folder does not even list back and
#                  every dashboard POST into it returns 500. Measured
#                  2026-09-09. Read from the environment or, preferred, from
#                  ~/.grafana_token so it never enters a shell history or a
#                  transcript.
#   GRAFANA_PROM   OPTIONAL: the NAME of the Prometheus datasource to bind the
#                  dashboards to. Omitted, each dashboard keeps its datasource
#                  variable and the viewer picks once from the dropdown.
#
# ⚠️ THE LOGS DASHBOARD IS EXCLUDED ON PURPOSE. truss-logs.json queries Loki,
# which this scope does not deploy -- "use the Prometheus there" is metrics.
# Importing it against a Grafana with no Loki would be a folder of panels that
# can never render. It is skipped by name below, not by luck.
#
# Idempotent: the folder is reused if it exists, and every dashboard is
# uploaded with overwrite=true, so re-running updates in place.
set -euo pipefail

cd "$(dirname "$0")"

url=${GRAFANA_URL:-}
[ -n "$url" ] || { echo "import: set GRAFANA_URL (e.g. https://grafana.example)" >&2; exit 2; }
url=${url%/}

if [ -n "${GRAFANA_TOKEN:-}" ]; then token="$GRAFANA_TOKEN"
elif [ -f "$HOME/.grafana_token" ]; then token=$(tr -d '\r\n' < "$HOME/.grafana_token")
else echo "import: no token -- set GRAFANA_TOKEN or write one to ~/.grafana_token (chmod 600)" >&2; exit 2; fi

command -v jq >/dev/null || { echo "import: jq is required" >&2; exit 2; }

api() { # method path [json-file]
  local method="$1" path="$2" body="${3:-}"
  if [ -n "$body" ]; then
    curl -fsS -X "$method" "$url/api$path" \
      -H "Authorization: Bearer $token" -H "Content-Type: application/json" \
      --data-binary "@$body"
  else
    curl -fsS -X "$method" "$url/api$path" -H "Authorization: Bearer $token"
  fi
}

# ⚠️ VERIFY REACHABILITY AND AUTH BEFORE WRITING ANYTHING. A folder half
# created against the wrong URL, or a 401 swallowed into a later confusing
# error, is worse than a clean stop here.
org=$(api GET /org 2>/dev/null | jq -r '.name // empty' || true)
[ -n "$org" ] || { echo "import: could not authenticate to $url -- check GRAFANA_URL and that the token is valid" >&2; exit 1; }
echo "import: authenticated to $url (org: $org)"

# The folder. Grafana's create-folder is not idempotent -- a second call is a
# 409 -- so look first.
folder_uid=$(api GET "/folders" | jq -r '.[] | select(.title=="Truss") | .uid' | head -1)
if [ -z "$folder_uid" ]; then
  folder_uid=$(printf '{"title":"Truss"}' | api POST /folders /dev/stdin | jq -r '.uid')
  # ⚠️ CONFIRM THE FOLDER LISTS BACK. An Editor token creates a folder it then
  # cannot use, and the only clean place to catch that is here -- before three
  # dashboard POSTs fail with an opaque 500 each. If the folder we just made is
  # not visible to us, the token cannot manage folders: it needs Admin.
  if ! api GET /folders | jq -e --arg u "$folder_uid" 'any(.uid==$u)' >/dev/null; then
    echo "import: created folder Truss ($folder_uid) but cannot see it -- the token is not Admin." >&2
    echo "import: an Editor token can create a folder it has no permission on. Use an Admin token." >&2
    exit 1
  fi
  echo "import: created folder Truss ($folder_uid)"
else
  echo "import: reusing folder Truss ($folder_uid)"
fi

count=0
for f in ../dashboards/*.json; do
  base=$(basename "$f")
  # ⚠️ metrics only; see the header on why the logs dashboard is skipped.
  case "$base" in *logs*) echo "import: skipping $base (Loki, out of scope)"; continue;; esac

  # Wrap the raw dashboard for POST /dashboards/db: strip any id/uid clash by
  # letting Grafana keep the file's uid, place it in the Truss folder, and
  # overwrite an existing copy. If GRAFANA_PROM is set, bind the datasource
  # variable's current value so the panels resolve without a manual pick.
  payload=$(mktemp); trap 'rm -f "$payload"' EXIT
  jq --arg uid "$folder_uid" --arg ds "${GRAFANA_PROM:-}" '
    (.templating.list // []) |= (map(
      if .type=="datasource" and ($ds|length)>0
      then . + {current:{text:$ds,value:$ds}} else . end))
    | {dashboard: (. + {id:null}), folderUid:$uid, overwrite:true, message:"truss import"}
  ' "$f" > "$payload"

  title=$(api POST /dashboards/db "$payload" | jq -r '.status // .message // "?"')
  echo "import: $base -> Truss ($title)"
  count=$((count+1))
done

echo "import: $count dashboards in the Truss folder at $url"
