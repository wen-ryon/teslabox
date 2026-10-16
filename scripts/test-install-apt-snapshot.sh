#!/bin/sh
set -eu

script_dir="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
test_dir="$(mktemp -d)"
trap 'find "${test_dir}" -depth -delete' EXIT HUP INT TERM

# 仅加载首装脚本的函数定义，避免测试触碰系统 APT 配置。
sed '/^trap cleanup EXIT HUP INT TERM/,$d' "${script_dir}/../deploy/install.sh" >"${test_dir}/functions.sh"
. "${test_dir}/functions.sh"
WORK_DIR="${test_dir}"
APT_DEPENDENCY_LISTS_DIR="${test_dir}/apt-dependency-lists"
DEBIAN_MAIN_MIRROR="https://deb.debian.org/debian"
DEBIAN_SECURITY_MIRROR="${DEBIAN_SECURITY_SNAPSHOT}"

replace_apt_source_file() {
    mv "${2}" "${1}"
}

assert_line() {
    awk -v expected="${2}" '$0 == expected { found = 1 } END { exit !found }' "${1}"
}

list_file="${test_dir}/sources.list"
printf '%s\n' \
    'deb https://security.debian.org/debian-security bullseye-security main contrib' \
    'deb https://deb.debian.org/debian bullseye main' \
    'deb https://example.invalid/custom stable main' >"${list_file}"
rewrite_debian_list_file "${list_file}"
assert_line "${list_file}" "deb [check-valid-until=no] ${DEBIAN_SECURITY_SNAPSHOT} bullseye-security main contrib"
assert_line "${list_file}" 'deb https://deb.debian.org/debian bullseye main'
assert_line "${list_file}" 'deb https://example.invalid/custom stable main'
cp "${list_file}" "${test_dir}/first.list"
rewrite_debian_list_file "${list_file}"
cmp -s "${list_file}" "${test_dir}/first.list"

printf '%s\n' \
    'deb [arch=arm64 signed-by=/usr/share/keyrings/debian-archive-keyring.gpg check-valid-until=yes] https://mirrors.ustc.edu.cn/debian-security bullseye-security main' >"${list_file}"
rewrite_debian_list_file "${list_file}"
assert_line "${list_file}" "deb [arch=arm64 signed-by=/usr/share/keyrings/debian-archive-keyring.gpg check-valid-until=no] ${DEBIAN_SECURITY_SNAPSHOT} bullseye-security main"

sources_file="${test_dir}/security.sources"
printf '%s\n' \
    'Types: deb' \
    'URIs: https://mirrors.aliyun.com/debian-security' \
    'Suites: bullseye-security' \
    'Components: main contrib' \
    '' >"${sources_file}"
rewrite_debian_deb822_file "${sources_file}"
assert_line "${sources_file}" "URIs: ${DEBIAN_SECURITY_SNAPSHOT}"
assert_line "${sources_file}" 'Check-Valid-Until: no'
cp "${sources_file}" "${test_dir}/first.sources"
rewrite_debian_deb822_file "${sources_file}"
cmp -s "${sources_file}" "${test_dir}/first.sources"

printf '%s\n' \
    'Types: deb' \
    'URIs: https://security.debian.org/debian-security' \
    'Suites: bullseye bullseye-security' \
    'Components: main' >"${sources_file}"
cp "${sources_file}" "${test_dir}/mixed.sources"
if rewrite_debian_deb822_file "${sources_file}" 2>/dev/null; then
    printf '%s\n' '混合 deb822 条目应被拒绝。' >&2
    exit 1
fi
cmp -s "${sources_file}" "${test_dir}/mixed.sources"

apt_events="${test_dir}/apt-events"
mkdir "${test_dir}/bin"
printf '%s\n' \
    '#!/bin/sh' \
    'printf "%s\n" "$*" >>"${APT_ARGUMENTS}"' \
    'case " $* " in' \
    '    *" update "*) printf "%s\n" update >>"${APT_EVENTS}"; [ "${APT_FAIL_UPDATE:-0}" -eq 0 ] ;;' \
    '    *" install -y -d "*)' \
    '        printf "%s\n" download >>"${APT_EVENTS}"' \
    '        [ "${APT_FAIL_DOWNLOAD:-0}" -eq 0 ] ;;' \
    '    *" install -y "*) printf "%s\n" install >>"${APT_EVENTS}" ;;' \
    '    *) exit 1 ;;' \
    'esac' >"${test_dir}/bin/apt-get"
chmod 0755 "${test_dir}/bin/apt-get"
APT_EVENTS="${apt_events}"
APT_ARGUMENTS="${test_dir}/apt-arguments"
PATH="${test_dir}/bin:${PATH}"
export APT_EVENTS APT_ARGUMENTS PATH

install_debian_dependencies avahi-daemon
printf '%s\n' update download install >"${test_dir}/expected-events"
cmp -s "${apt_events}" "${test_dir}/expected-events"
assert_line "${APT_DEPENDENCY_SOURCE_LIST}" "deb [signed-by=/usr/share/keyrings/debian-archive-keyring.gpg] ${DEBIAN_MAIN_MIRROR} bullseye main contrib non-free"
assert_line "${APT_DEPENDENCY_SOURCE_LIST}" "deb [signed-by=/usr/share/keyrings/debian-archive-keyring.gpg check-valid-until=no] ${DEBIAN_SECURITY_SNAPSHOT} bullseye-security main contrib non-free"
assert_line "${APT_DEPENDENCY_SOURCE_LIST}" 'deb [signed-by=/usr/share/keyrings/radxa-archive-keyring.gpg] https://radxa-repo.github.io/bullseye rockchip-bullseye main'
awk -v list="${APT_DEPENDENCY_SOURCE_LIST}" -v state="${APT_DEPENDENCY_LISTS_DIR}" '
    index($0, "Dir::Etc::sourcelist=" list) == 0 ||
    index($0, "Dir::Etc::sourceparts=-") == 0 ||
    index($0, "Dir::State::lists=" state) == 0 { failed = 1 }
    END { exit (failed || NR != 3) }
' "${APT_ARGUMENTS}"
if awk '/tailscale|vscodium|trusted=yes|allow-insecure/ { found = 1 } END { exit !found }' "${APT_DEPENDENCY_SOURCE_LIST}"; then
    printf '%s\n' '第三方源或签名绕过选项不应进入首装依赖源。' >&2
    exit 1
fi

: >"${apt_events}"
if (APT_FAIL_UPDATE=1; export APT_FAIL_UPDATE; install_debian_dependencies avahi-daemon) >/dev/null 2>&1; then
    printf '%s\n' '必需源刷新失败时不得继续下载或安装。' >&2
    exit 1
fi
printf '%s\n' update >"${test_dir}/expected-events"
cmp -s "${apt_events}" "${test_dir}/expected-events"

: >"${apt_events}"
if (APT_FAIL_DOWNLOAD=1; export APT_FAIL_DOWNLOAD; install_debian_dependencies avahi-daemon) >/dev/null 2>&1; then
    printf '%s\n' '依赖下载失败时不得继续安装。' >&2
    exit 1
fi
printf '%s\n' update download >"${test_dir}/expected-events"
cmp -s "${apt_events}" "${test_dir}/expected-events"

printf '%s\n' 'APT 快照源改写测试通过。'
