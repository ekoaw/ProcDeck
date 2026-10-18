#!/bin/sh
# php is read from the "php =" line of procdeck.ini; a Windows php.exe there
# means the ini is shared with Windows, so php from the PATH is used instead.
dir=$(cd "$(dirname "$0")" && pwd)
php=$(sed -n 's/^[[:space:]]*php[[:space:]]*=[[:space:]]*//p' "$dir/procdeck.ini" | head -n 1 | tr -d '\r"')
case "$php" in
    '' | *.exe | *.EXE) php=php ;;
esac
if ! "$php" -v >/dev/null 2>&1; then
    echo "PHP not found: $php"
    echo "Set the \"php =\" line in $dir/procdeck.ini to the location of php, for example /usr/bin/php"
    exit 1
fi
exec "$php" "$dir/manager.php"
