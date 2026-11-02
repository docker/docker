#!/bin/sh
# dockerd-rootless-setuptool.sh: setup tool for dockerd-rootless.sh
# Needs to be executed as a non-root user.
#
# Typical usage: dockerd-rootless-setuptool.sh install --force
#
# Documentation: https://docs.docker.com/go/rootless/
set -eu

# utility functions
INFO() {
	/bin/echo -e "\e[104m\e[97m[INFO]\e[49m\e[39m $@"
}

WARNING() {
	/bin/echo >&2 -e "\e[101m\e[97m[WARNING]\e[49m\e[39m $@"
}

ERROR() {
	/bin/echo >&2 -e "\e[101m\e[97m[ERROR]\e[49m\e[39m $@"
}

# constants
DOCKERD_ROOTLESS_SH="dockerd-rootless.sh"
SYSTEMD_UNIT="docker.service"
CLI_CONTEXT="rootless"

# CLI opt: --force
OPT_FORCE=""
# CLI opt: --skip-iptables
OPT_SKIP_IPTABLES=""

# global vars
ARG0="$0"
DOCKERD_ROOTLESS_SH_FLAGS=""
BIN=""
SYSTEMD=""
CFG_DIR=""
XDG_RUNTIME_DIR_CREATED=""
USERNAME=""
USERNAME_ESCAPED=""

# run checks and also initialize global vars
init() {
	# OS verification: Linux only
	case "$(uname)" in
		Linux) ;;

		*)
			ERROR "Rootless Docker cannot be installed on $(uname)"
			exit 1
			;;
	esac

	# User verification: deny running as root
	if [ "$(id -u)" = "0" ]; then
		ERROR "Refusing to install rootless Docker as the root user"
		exit 1
	fi

	# set BIN
	if ! BIN="$(command -v "$DOCKERD_ROOTLESS_SH" 2> /dev/null)"; then
		ERROR "$DOCKERD_ROOTLESS_SH needs to be present under \$PATH"
		exit 1
	fi
	BIN=$(dirname "$BIN")

	# set SYSTEMD
	if systemctl --user show-environment > /dev/null 2>&1; then
		SYSTEMD=1
	fi

	# HOME verification
	if [ -z "${HOME:-}" ] || [ ! -d "$HOME" ]; then
		ERROR "HOME needs to be set"
		exit 1
	fi
	if [ ! -w "$HOME" ]; then
		ERROR "HOME needs to be writable"
		exit 1
	fi

	# Set USERNAME from `id -un` and potentially protect backslash
	# for windbind/samba domain users
	USERNAME=$(id -un)
	USERNAME_ESCAPED=$(echo $USERNAME | sed 's/\\/\\\\/g')

	# set CFG_DIR
	CFG_DIR="${XDG_CONFIG_HOME:-$HOME/.config}"

	# Existing rootful docker verification
	if [ -w /var/run/docker.sock ] && [ -z "$OPT_FORCE" ]; then
		ERROR "Aborting because rootful Docker (/var/run/docker.sock) is running and accessible. Set --force to ignore."
		exit 1
	fi

	# Validate XDG_RUNTIME_DIR and set XDG_RUNTIME_DIR_CREATED
	if [ -z "${XDG_RUNTIME_DIR:-}" ] || [ ! -w "$XDG_RUNTIME_DIR" ]; then
		if [ -n "$SYSTEMD" ]; then
			ERROR "Aborting because systemd was detected but XDG_RUNTIME_DIR (\"$XDG_RUNTIME_DIR\") is not set, does not exist, or is not writable"
			ERROR "Hint: this could happen if you changed users with 'su' or 'sudo'. To work around this:"
			ERROR "- try again by first running with root privileges 'loginctl enable-linger <user>' where <user> is the unprivileged user and export XDG_RUNTIME_DIR to the value of RuntimePath as shown by 'loginctl show-user <user>'"
			ERROR "- or simply log back in as the desired unprivileged user (ssh works for remote machines, machinectl shell works for local machines)"
			exit 1
		fi
		export XDG_RUNTIME_DIR="$HOME/.docker/run"
		mkdir -p -m 700 "$XDG_RUNTIME_DIR"
		XDG_RUNTIME_DIR_CREATED=1
	fi

	: "${CONTAINERD_ROOTLESS_ROOTLESSKIT_STATE_DIR:=$XDG_RUNTIME_DIR/containerd-rootless}"
	if [ -e "$CONTAINERD_ROOTLESS_ROOTLESSKIT_STATE_DIR" ]; then
		# https://github.com/moby/moby/issues/52171
		# Hard requirement, not bypassable with --force
		ERROR "dockerd-rootless.sh conflicts with containerd-rootless.sh. Stop containerd-rootless.sh if it's running, and remove $CONTAINERD_ROOTLESS_ROOTLESSKIT_STATE_DIR if it still exists."
		exit 1
	fi

	instructions=""
	# instruction: uidmap dependency check
	if ! command -v newuidmap > /dev/null 2>&1; then
		if command -v apt-get > /dev/null 2>&1; then
			instructions=$(
				cat <<- EOI
					${instructions}
					# Install newuidmap & newgidmap binaries
					apt-get install -y uidmap
				EOI
			)
		elif command -v dnf > /dev/null 2>&1; then
			instructions=$(
				cat <<- EOI
					${instructions}
					# Install newuidmap & newgidmap binaries
					dnf install -y shadow-utils
				EOI
			)
		elif command -v yum > /dev/null 2>&1; then
			instructions=$(
				cat <<- EOI
					${instructions}
					# Install newuidmap & newgidmap binaries
					yum install -y shadow-utils
				EOI
			)
		else
			ERROR "newuidmap binary not found. Please install with a package manager."
			exit 1
		fi
	fi

	# instruction: iptables dependency check
	faced_iptables_error=""
	# Many OSs now use iptables-nft by default so, check for module nf_tables by default. But,
	# if "iptables --version" worked and reported "legacy", check for module ip_tables instead.
	iptables_module="nf_tables"
	iptables_command=$(PATH=$PATH:/sbin:/usr/sbin command -v iptables 2> /dev/null) || :
	if [ -n "$iptables_command" ]; then
		iptables_version=$($iptables_command --version 2> /dev/null) || :
		case $iptables_version in
			*legacy*) iptables_module="ip_tables" ;;
		esac
	else
		faced_iptables_error=1
		if [ -z "$OPT_SKIP_IPTABLES" ]; then
			if command -v apt-get > /dev/null 2>&1; then
				instructions=$(
					cat <<- EOI
						${instructions}
						# Install iptables
						apt-get install -y iptables
					EOI
				)
			elif command -v dnf > /dev/null 2>&1; then
				instructions=$(
					cat <<- EOI
						${instructions}
						# Install iptables
						dnf install -y iptables
					EOI
				)
			elif command -v yum > /dev/null 2>&1; then
				instructions=$(
					cat <<- EOI
						${instructions}
						# Install iptables
						yum install -y iptables
					EOI
				)
			else
				ERROR "iptables binary not found. Please install with a package manager."
				exit 1
			fi
		fi
	fi

	# instruction: ip_tables module dependency check
	if ! grep -q $iptables_module /proc/modules 2> /dev/null && ! grep -q $iptables_module /lib/modules/$(uname -r)/modules.builtin 2> /dev/null; then
		faced_iptables_error=1
		if [ -z "$OPT_SKIP_IPTABLES" ]; then
			instructions=$(
				cat <<- EOI
					${instructions}
					# Load $iptables_module module
					modprobe $iptables_module
				EOI
			)
		fi
	fi

	# set DOCKERD_ROOTLESS_SH_FLAGS
	if [ -n "$faced_iptables_error" ] && [ -n "$OPT_SKIP_IPTABLES" ]; then
		DOCKERD_ROOTLESS_SH_FLAGS="${DOCKERD_ROOTLESS_SH_FLAGS} --iptables=false"
	fi

	# instruction: Debian and Arch require setting unprivileged_userns_clone
	if [ -f /proc/sys/kernel/unprivileged_userns_clone ]; then
		if [ "1" != "$(cat /proc/sys/kernel/unprivileged_userns_clone)" ]; then
			instructions=$(
				cat <<- EOI
					${instructions}
					# Set kernel.unprivileged_userns_clone
					cat <<EOT > /etc/sysctl.d/50-rootless.conf
					kernel.unprivileged_userns_clone = 1
					EOT
					sysctl --system
				EOI
			)
		fi
	fi

	# instructions: validate subuid for current user
	error_subid=
	if command -v "getsubids" > /dev/null 2>&1; then
		getsubids "$USERNAME" > /dev/null 2>&1 || getsubids "$(id -u)" > /dev/null 2>&1 || error_subid=1
	else
		grep -q "^$USERNAME_ESCAPED:\|^$(id -u):" /etc/subuid 2> /dev/null || error_subid=1
	fi
	if [ "$error_subid" = "1" ]; then
		instructions=$(
			cat <<- EOI
				${instructions}
				# Add subuid entry for ${USERNAME}
				echo "${USERNAME}:100000:65536" >> /etc/subuid
			EOI
		)
	fi

	# instructions: validate subgid for current user
	error_subid=
	if command -v "getsubids" > /dev/null 2>&1; then
		getsubids -g "$USERNAME" > /dev/null 2>&1 || getsubids -g "$(id -u)" > /dev/null 2>&1 || error_subid=1
	else
		grep -q "^$USERNAME_ESCAPED:\|^$(id -u):" /etc/subgid 2> /dev/null || error_subid=1
	fi
	if [ "$error_subid" = "1" ]; then
		instructions=$(
			cat <<- EOI
				${instructions}
				# Add subgid entry for ${USERNAME}
				echo "${USERNAME}:100000:65536" >> /etc/subgid
			EOI
		)
	fi

	# fail with instructions if requirements are not satisfied.
	if [ -n "$instructions" ]; then
		ERROR "Missing system requirements. Run the following commands to"
		ERROR "install the requirements and run this tool again."
		if [ -n "$faced_iptables_error" ] && [ -z "$OPT_SKIP_IPTABLES" ]; then
			ERROR "Alternatively iptables checks can be disabled with --skip-iptables ."
		fi
		echo
		echo "########## BEGIN ##########"
		echo "sudo sh -eux <<EOF"
		echo "$instructions" | sed -e '/^$/d'
		echo "EOF"
		echo "########## END ##########"
		echo
		exit 1
	fi
	# TODO: support printing non-essential but recommended instructions:
	# - sysctl: "net.ipv4.ping_group_range"
	# - sysctl: "net.ipv4.ip_unprivileged_port_start"
	# - external binary: fuse-overlayfs
}

# CLI subcommand: "check"
cmd_entrypoint_check() {
	init
	# requirements are already checked in init()
	INFO "Requirements are satisfied"
}

# CLI subcommand: "nsenter"
cmd_entrypoint_nsenter() {
	# No need to call init()
	pid=$(cat "$XDG_RUNTIME_DIR/dockerd-rootless/child_pid")
	n=""
	# If RootlessKit is running with `--detach-netns` mode, we do NOT enter the detached netns here
	if [ ! -e "$XDG_RUNTIME_DIR/dockerd-rootless/netns" ]; then
		n="-n"
	fi
	exec nsenter --no-fork --wd="$(pwd)" --preserve-credentials -m $n -U -t "$pid" -- "$@"
}

show_systemd_error() {
	n="20"
	ERROR "Failed to start ${SYSTEMD_UNIT}. Run \`journalctl -n ${n} --no-pager --user --unit ${SYSTEMD_UNIT}\` to show the error log."
	ERROR "Before retrying installation, you might need to uninstall the current setup: \`$0 uninstall -f ; ${BIN}/rootlesskit rm -rf ${HOME}/.local/share/docker\`"
	if journalctl -q -n ${n} --user --unit ${SYSTEMD_UNIT} | grep -qF "/run/xtables.lock: Permission denied"; then
		ERROR "Failure likely related to https://github.com/moby/moby/issues/41230"
		ERROR "This may work as a workaround: \`sudo dnf install -y policycoreutils-python-utils && sudo semanage permissive -a iptables_t\`"
	fi
}

# install (systemd)
install_systemd() {
	mkdir -p "${CFG_DIR}/systemd/user"
	unit_file="${CFG_DIR}/systemd/user/${SYSTEMD_UNIT}"
	if [ -f "${unit_file}" ]; then
		WARNING "File already exists, skipping: ${unit_file}"
	else
		INFO "Creating ${unit_file}"
		cat <<- EOT > "${unit_file}"
			[Unit]
			Description=Docker Application Container Engine (Rootless)
			Documentation=https://docs.docker.com/go/rootless/
			Requires=dbus.socket

			[Service]
			Environment=PATH=$BIN:/sbin:/usr/sbin:$PATH
			ExecStart=$BIN/dockerd-rootless.sh $DOCKERD_ROOTLESS_SH_FLAGS
			ExecReload=/bin/kill -s HUP \$MAINPID
			TimeoutSec=0
			RestartSec=2
			Restart=always
			StartLimitBurst=3
			StartLimitInterval=60s
			LimitNOFILE=infinity
			LimitNPROC=infinity
			LimitCORE=infinity
			TasksMax=infinity
			Delegate=yes
			Type=notify
			NotifyAccess=all
			KillMode=mixed

			[Install]
			WantedBy=default.target
		EOT
		systemctl --user daemon-reload
	fi
	if ! systemctl --user --no-pager status "${SYSTEMD_UNIT}" > /dev/null 2>&1; then
		INFO "starting systemd service ${SYSTEMD_UNIT}"
		(
			set -x
			if ! systemctl --user start "${SYSTEMD_UNIT}"; then
				set +x
				show_systemd_error
				exit 1
			fi
			sleep 3
		)
	fi
	(
		set -x
		if ! systemctl --user --no-pager --full status "${SYSTEMD_UNIT}"; then
			set +x
			show_systemd_error
			exit 1
		fi
		DOCKER_HOST="unix://$XDG_RUNTIME_DIR/docker.sock" $BIN/docker version
		systemctl --user enable "${SYSTEMD_UNIT}"
	)
	INFO "Installed ${SYSTEMD_UNIT} successfully."
	INFO "To control ${SYSTEMD_UNIT}, run: \`systemctl --user (start|stop|restart) ${SYSTEMD_UNIT}\`"
	INFO "To run ${SYSTEMD_UNIT} on system startup, run: \`sudo loginctl enable-linger ${USERNAME}\`"
	echo
}

# install (non-systemd)
install_nonsystemd() {
	INFO "systemd not detected, ${DOCKERD_ROOTLESS_SH} needs to be started manually:"
	echo
	echo "PATH=$BIN:/sbin:/usr/sbin:\$PATH ${DOCKERD_ROOTLESS_SH} ${DOCKERD_ROOTLESS_SH_FLAGS}"
	echo
}

cli_ctx_exists() {
	name="$1"
	"${BIN}/docker" --context=default context inspect -f "{{.Name}}" "${name}" > /dev/null 2>&1
}

cli_ctx_create() {
	name="$1"
	host="$2"
	description="$3"
	"${BIN}/docker" --context=default context create "${name}" --docker "host=${host}" --description "${description}" > /dev/null
}

cli_ctx_use() {
	name="$1"
	"${BIN}/docker" --context=default context use "${name}" > /dev/null
}

cli_ctx_rm() {
	name="$1"
	"${BIN}/docker" --context=default context rm -f "${name}" > /dev/null
}

# CLI subcommand: "install"
cmd_entrypoint_install() {
	init
	# Most requirements are already checked in init(), except the smoke test below for RootlessKit.
	# https://github.com/docker/docker-install/issues/417

	# check RootlessKit functionality. RootlessKit will print hints if something is still unsatisfied.
	# (e.g., `kernel.apparmor_restrict_unprivileged_userns` constraint)
	if ! rootlesskit true; then
		if [ -z "$OPT_FORCE" ]; then
			ERROR "RootlessKit failed, see the error messages and https://rootlesscontaine.rs/getting-started/common/ . Set --force to ignore."
			exit 1
		else
			WARNING "RootlessKit failed, see the error messages and https://rootlesscontaine.rs/getting-started/common/ ."
		fi
	fi

	if [ -z "$SYSTEMD" ]; then
		install_nonsystemd
	else
		install_systemd
	fi

	if cli_ctx_exists "${CLI_CONTEXT}"; then
		INFO "CLI context \"${CLI_CONTEXT}\" already exists"
	else
		INFO "Creating CLI context \"${CLI_CONTEXT}\""
		cli_ctx_create "${CLI_CONTEXT}" "unix://${XDG_RUNTIME_DIR}/docker.sock" "Rootless mode"
	fi

	INFO "Using CLI context \"${CLI_CONTEXT}\""
	cli_ctx_use "${CLI_CONTEXT}"

	echo
	INFO "Make sure the following environment variable(s) are set (or add them to ~/.bashrc):"
	if [ -n "$XDG_RUNTIME_DIR_CREATED" ]; then
		echo "# WARNING: systemd not found. You have to remove XDG_RUNTIME_DIR manually on every logout."
		echo "export XDG_RUNTIME_DIR=${XDG_RUNTIME_DIR}"
	fi
	echo "export PATH=${BIN}:\$PATH"
	echo
	INFO "Some applications may require the following environment variable too:"
	echo "export DOCKER_HOST=unix://${XDG_RUNTIME_DIR}/docker.sock"
	echo

}

# CLI subcommand: "uninstall"
cmd_entrypoint_uninstall() {
	init
	# requirements are already checked in init()
	if [ -z "$SYSTEMD" ]; then
		INFO "systemd not detected, ${DOCKERD_ROOTLESS_SH} needs to be stopped manually:"
	else
		unit_file="${CFG_DIR}/systemd/user/${SYSTEMD_UNIT}"
		(
			set -x
			systemctl --user stop "${SYSTEMD_UNIT}"
		) || :
		(
			set -x
			systemctl --user disable "${SYSTEMD_UNIT}"
		) || :
		rm -f "${unit_file}"
		INFO "Uninstalled ${SYSTEMD_UNIT}"
	fi

	if cli_ctx_exists "${CLI_CONTEXT}"; then
		cli_ctx_rm "${CLI_CONTEXT}"
		INFO "Deleted CLI context \"${CLI_CONTEXT}\""
	fi
	unset DOCKER_HOST
	unset DOCKER_CONTEXT
	cli_ctx_use "default"
	INFO 'Configured CLI to use the "default" context.'
	INFO
	INFO 'Make sure to unset or update the environment PATH, DOCKER_HOST, and DOCKER_CONTEXT environment variables if you have added them to `~/.bashrc`.'
	INFO "This uninstallation tool does NOT remove Docker binaries and data."
	INFO "To remove data, run: \`$BIN/rootlesskit rm -rf $HOME/.local/share/docker\`"
}

# text for --help
usage() {
	echo "Usage: ${ARG0} [OPTIONS] COMMAND"
	echo
	echo "A setup tool for Rootless Docker (${DOCKERD_ROOTLESS_SH})."
	echo
	echo "Documentation: https://docs.docker.com/go/rootless/"
	echo
	echo "Options:"
	echo "  -f, --force                Ignore rootful Docker (/var/run/docker.sock)"
	echo "      --skip-iptables        Ignore missing iptables"
	echo
	echo "Commands:"
	echo "  check        Check prerequisites"
	echo "  nsenter      Enter into RootlessKit namespaces (mostly for debugging)"
	echo "  install      Install systemd unit (if systemd is available) and show how to manage the service"
	echo "  uninstall    Uninstall systemd unit"
}

# parse CLI args
if ! args="$(getopt -o hf --long help,force,skip-iptables -n "$ARG0" -- "$@")"; then
	usage
	exit 1
fi
eval set -- "$args"
while [ "$#" -gt 0 ]; do
	arg="$1"
	shift
	case "$arg" in
		-h | --help)
			usage
			exit 0
			;;
		-f | --force)
			OPT_FORCE=1
			;;
		--skip-iptables)
			OPT_SKIP_IPTABLES=1
			;;
		--)
			break
			;;
		*)
			# XXX this means we missed something in our "getopt" arguments above!
			ERROR "Scripting error, unknown argument '$arg' when parsing script arguments."
			exit 1
			;;
	esac
done

command="${1:-}"
if [ -z "$command" ]; then
	ERROR "No command was specified. Run with --help to see the usage. Maybe you want to run \`$ARG0 install\`?"
	exit 1
fi

if ! command -v "cmd_entrypoint_${command}" > /dev/null 2>&1; then
	ERROR "Unknown command: ${command}. Run with --help to see the usage."
	exit 1
fi

# main
shift
"cmd_entrypoint_${command}" "$@"
