#!/bin/sh
# Tofu (2026-10-23): this file is Tofu's addition; it is not part of upstream Homebox.
#
# Maps the variables Tofu sets on every app onto Homebox's own HBOX_* settings, then starts the server:
#   PORT          -> HBOX_WEB_PORT. Tofu sends traffic to PORT, so PORT wins over HBOX_WEB_PORT; without PORT
#                    the upstream default (7745) stays.
#   DATABASE_URL  -> HBOX_DATABASE_DRIVER=postgres plus HBOX_DATABASE_HOST, _PORT, _USERNAME, _PASSWORD,
#                    _DATABASE and _SSL_MODE. An HBOX_DATABASE_* value that is already set is kept as it is.
set -eu

# Decodes the %XX escapes of one URL component: DATABASE_URL percent-encodes its user, password and database.
url_decode() {
	printf '%b' "$(printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/%\([0-9A-Fa-f][0-9A-Fa-f]\)/\\x\1/g')"
}

if [ -n "${PORT:-}" ]; then
	HBOX_WEB_PORT=$PORT
	export HBOX_WEB_PORT
fi

if [ -n "${DATABASE_URL:-}" ]; then
	case $DATABASE_URL in
	postgres://* | postgresql://*)
		# postgresql://user:password@host:port/database?sslmode=require
		rest=${DATABASE_URL#*://}

		query=
		case $rest in
		*\?*)
			query=${rest#*\?}
			rest=${rest%%\?*}
			;;
		esac

		database=
		case $rest in
		*/*)
			database=${rest#*/}
			rest=${rest%%/*}
			;;
		esac

		userinfo=
		case $rest in
		*@*)
			userinfo=${rest%@*}
			rest=${rest##*@}
			;;
		esac
		user=${userinfo%%:*}
		password=
		case $userinfo in
		*:*) password=${userinfo#*:} ;;
		esac

		host=$rest
		port=
		case $rest in
		\[*\]) ;;
		\[*\]:* | *:*)
			host=${rest%:*}
			port=${rest##*:}
			;;
		esac
		host=${host#\[}
		host=${host%\]}

		sslmode=
		query="&$query"
		case $query in
		*'&sslmode='*)
			sslmode=${query#*'&sslmode='}
			sslmode=${sslmode%%'&'*}
			;;
		esac

		: "${HBOX_DATABASE_DRIVER:=postgres}"
		: "${HBOX_DATABASE_HOST:=$host}"
		: "${HBOX_DATABASE_PORT:=${port:-5432}}"
		: "${HBOX_DATABASE_USERNAME:=$(url_decode "$user")}"
		: "${HBOX_DATABASE_PASSWORD:=$(url_decode "$password")}"
		export HBOX_DATABASE_DRIVER HBOX_DATABASE_HOST HBOX_DATABASE_PORT HBOX_DATABASE_USERNAME HBOX_DATABASE_PASSWORD
		if [ -n "$database" ]; then
			: "${HBOX_DATABASE_DATABASE:=$(url_decode "$database")}"
			export HBOX_DATABASE_DATABASE
		fi
		if [ -n "$sslmode" ]; then
			: "${HBOX_DATABASE_SSL_MODE:=$sslmode}"
			export HBOX_DATABASE_SSL_MODE
		fi
		;;
	*)
		echo "tofu-entrypoint: DATABASE_URL is not a postgres:// URL, so the HBOX_DATABASE_* settings are left as they are" >&2
		;;
	esac
fi

exec /app/api "$@"
