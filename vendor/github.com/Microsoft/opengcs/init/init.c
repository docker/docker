#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <getopt.h>
#include <net/if.h>
#include <netinet/ip.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mount.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/sysmacros.h>
#include <sys/types.h>
#include <sys/wait.h>
#include <unistd.h>

#define DEFAULT_PATH_ENV "PATH=/sbin:/usr/sbin:/bin:/usr/bin"

const char *const default_envp[] = {
    DEFAULT_PATH_ENV,
    NULL,
};

// When nothing is passed, default to the LCOWv1 behavior.
const char *const default_argv[] = { "/bin/gcs", "-loglevel", "debug", "-logfile=/tmp/gcs.log" };
const char *const default_shell = "/bin/sh";

struct Mount {
    const char *source, *target, *type;
    unsigned long flags;
    const void *data;
};

struct Mkdir {
    const char *path;
    mode_t mode;
};

struct Mknod {
    const char *path;
    mode_t mode;
    int major, minor;
};

struct Symlink {
    const char *linkpath, *target;
};

enum OpType {
    OpMount,
    OpMkdir,
    OpMknod,
    OpSymlink,
};

struct InitOp {
    enum OpType op;
    union {
        struct Mount mount;
        struct Mkdir mkdir;
        struct Mknod mknod;
        struct Symlink symlink;
    };
};

const struct InitOp ops[] = {
    // mount /proc (which should already exist)
    { OpMount, .mount = { "proc", "/proc", "proc", MS_NODEV | MS_NOSUID | MS_NOEXEC } },

    // add symlinks in /dev (which is already mounted)
    { OpSymlink, .symlink = { "/dev/fd", "/proc/self/fd" } },
    { OpSymlink, .symlink = { "/dev/stdin", "/proc/self/fd/0" } },
    { OpSymlink, .symlink = { "/dev/stdout", "/proc/self/fd/1" } },
    { OpSymlink, .symlink = { "/dev/stderr", "/proc/self/fd/2" } },

    // mount tmpfs on /run and /tmp (which should already exist)
    { OpMount, .mount = { "tmpfs", "/run", "tmpfs", MS_NODEV | MS_NOSUID | MS_NOEXEC, "mode=0755" } },
    { OpMount, .mount = { "tmpfs", "/tmp", "tmpfs", MS_NODEV | MS_NOSUID | MS_NOEXEC } },

    // mount shm and devpts
    { OpMkdir, .mkdir = { "/dev/shm", 0755 } },
    { OpMount, .mount = { "shm", "/dev/shm", "tmpfs", MS_NODEV | MS_NOSUID | MS_NOEXEC } },
    { OpMkdir, .mkdir = { "/dev/pts", 0755 } },
    { OpMount, .mount = { "devpts", "/dev/pts", "devpts", MS_NOSUID | MS_NOEXEC } },

    // mount /sys (which should already exist)
    { OpMount, .mount = { "sysfs", "/sys", "sysfs", MS_NODEV | MS_NOSUID | MS_NOEXEC } },
    { OpMount, .mount = { "cgroup_root", "/sys/fs/cgroup", "tmpfs", MS_NODEV | MS_NOSUID | MS_NOEXEC, "mode=0755" } },
};

void warn(const char *msg) {
    int error = errno;
    perror(msg);
    errno = error;
}

void warn2(const char *msg1, const char *msg2) {
    int error = errno;
    fputs(msg1, stderr);
    fputs(": ", stderr);
    errno = error;
    warn(msg2);
}

_Noreturn void dien() {
    exit(errno);
}

_Noreturn void die(const char *msg) {
    warn(msg);
    dien();
}

_Noreturn void die2(const char *msg1, const char *msg2) {
    warn2(msg1, msg2);
    dien();
}

void init_dev() {
    if (mount("dev", "/dev", "devtmpfs", MS_NOSUID | MS_NOEXEC, NULL) < 0) {
        warn2("mount", "/dev");
        // /dev will be already mounted if devtmpfs.mount = 1 on the kernel
        // command line or CONFIG_DEVTMPFS_MOUNT is set. Do not consider this
        // an error.
        if (errno != EBUSY) {
            dien();
        }
    }
}

void init_fs(const struct InitOp *ops, size_t count) {
    for (size_t i = 0; i < count; i++) {
        switch (ops[i].op) {
        case OpMount: {
            const struct Mount *m = &ops[i].mount;
            if (mount(m->source, m->target, m->type, m->flags, m->data) < 0) {
                die2("mount", m->target);
            }
            break;
        }
        case OpMkdir: {
            const struct Mkdir *m = &ops[i].mkdir;
            if (mkdir(m->path, m->mode) < 0) {
                warn2("mkdir", m->path);
                if (errno != EEXIST) {
                    dien();
                }
            }
            break;
        }
        case OpMknod: {
            const struct Mknod *n = &ops[i].mknod;
            if (mknod(n->path, n->mode, makedev(n->major, n->minor)) < 0) {
                warn2("mknod", n->path);
                if (errno != EEXIST) {
                    dien();
                }
            }
            break;
        }
        case OpSymlink: {
            const struct Symlink *sl = &ops[i].symlink;
            if (symlink(sl->target, sl->linkpath) < 0) {
                warn2("symlink", sl->linkpath);
                if (errno != EEXIST) {
                    dien();
                }
            }
            break;
        }
        }
    }
}

void init_cgroups() {
    const char *fpath = "/proc/cgroups";
    FILE *f = fopen(fpath, "r");
    if (f == NULL) {
        die2("fopen", fpath);
    }
    // Skip the first line.
    for (;;) {
        char c = fgetc(f);
        if (c == EOF || c == '\n') {
            break;
        }
    }
    for (;;) {
        static const char base_path[] = "/sys/fs/cgroup/";
        char path[sizeof(base_path) - 1 + 64];
        char* name = path + sizeof(base_path) - 1;
        int hier, groups, enabled;
        int r = fscanf(f, "%64s %d %d %d\n", name, &hier, &groups, &enabled);
        if (r == EOF) {
            break;
        }
        if (r != 4) {
            errno = errno ? : EINVAL;
            die2("fscanf", fpath);
        }
        if (enabled) {
            memcpy(path, base_path, sizeof(base_path) - 1);
            if (mkdir(path, 0755) < 0) {
                die2("mkdir", path);
            }
            if (mount(name, path, "cgroup", MS_NODEV | MS_NOSUID | MS_NOEXEC, name) < 0) {
                die2("mount", path);
            }
        }
    }
    fclose(f);
}

void init_network(const char *iface, int domain) {
    int s = socket(domain, SOCK_DGRAM, IPPROTO_IP);
    if (s < 0) {
        if (errno == EAFNOSUPPORT) {
            return;
        }
        die("socket");
    }

    struct ifreq request = {0};
    strncpy(request.ifr_name, iface, sizeof(request.ifr_name));
    if (ioctl(s, SIOCGIFFLAGS, &request) < 0) {
        die2("ioctl(SIOCGIFFLAGS)", iface);
    }

    request.ifr_flags |= IFF_UP | IFF_RUNNING;
    if (ioctl(s, SIOCSIFFLAGS, &request) < 0) {
        die2("ioctl(SIOCSIFFLAGS)", iface);
    }

    close(s);
}

pid_t launch(int argc, char **argv) {
    int pid = fork();
    if (pid != 0) {
        if (pid < 0) {
            die("fork");
        }

        return pid;
    }

    // Unblock signals before execing.
    sigset_t set;
    sigfillset(&set);
    sigprocmask(SIG_UNBLOCK, &set, 0);

    // Create a session and process group.
    setsid();
    setpgid(0, 0);

    // Terminate the arguments and exec.
    char **argvn = alloca(sizeof(argv[0]) * (argc + 1));
    memcpy(argvn, argv, sizeof(argv[0]) * argc);
    argvn[argc] = NULL;
    if (putenv(DEFAULT_PATH_ENV)) { // Specify the PATH used for execvpe
        die("putenv");
    }
    execvpe(argvn[0], argvn, (char**)default_envp);
    die2("execvpe", argvn[0]);
}

int reap_until(pid_t until_pid) {
    for (;;) {
        int status;
        pid_t pid = wait(&status);
        if (pid < 0) {
            die("wait");
        }

        if (pid == until_pid) {
            // The initial child process died. Pass through the exit status.
            if (WIFEXITED(status)) {
                if (WEXITSTATUS(status) != 0) {
                    fputs("child exited with error\n", stderr);
                }
                return WEXITSTATUS(status);
            }
            fputs("child exited by signal\n", stderr);
            return 128 + WTERMSIG(status);
        }
    }
}

int main(int argc, char **argv) {
    char *debug_shell = NULL;
    if (argc <= 1) {
        argv = (char **)default_argv;
        argc = sizeof(default_argv) / sizeof(default_argv[0]);
        optind = 0;
        debug_shell = (char*)default_shell;
    } else {
        for (int opt; (opt = getopt(argc, argv, "+d:")) >= 0; ) {
            switch (opt) {
            case 'd':
                debug_shell = optarg;
                break;

            default:
                exit(1);
            }
        }
    }

    char **child_argv = argv + optind;
    int child_argc = argc - optind;

    // Block all signals in init. SIGCHLD will still cause wait() to return.
    sigset_t set;
    sigfillset(&set);
    sigprocmask(SIG_BLOCK, &set, 0);

    init_dev();
    init_fs(ops, sizeof(ops) / sizeof(ops[0]));
    init_cgroups();
    init_network("lo", AF_INET);
    init_network("lo", AF_INET6);

    pid_t pid = launch(child_argc, child_argv);
    if (debug_shell != NULL) {
        // The debug shell takes over as the primary child.
        pid = launch(1, &debug_shell);
    }

    // Reap until the initial child process dies.
    return reap_until(pid);
}
