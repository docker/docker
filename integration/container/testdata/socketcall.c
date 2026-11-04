#include <stdio.h>
#include <errno.h>
#include <stdint.h>
#include <signal.h>
#include <string.h>
#include <unistd.h>
#include <sys/socket.h>
#include <sys/mman.h>
#include <sys/resource.h>
#include <sys/wait.h>

#define SYS_GETPID_I386 20
#define SYS_SOCKETCALL_I386 102
#define SYS_SOCKET 1
#define SYS_CONNECT 3

/* Exit code reported when the kernel has no ia32 emulation. */
#define IA32_UNAVAILABLE_EXIT_CODE 77

#ifndef SOCK_FAMILY
#error "define SOCK_FAMILY via -DSOCK_FAMILY=..."
#endif
#ifndef SOCK_TYPE
#error "define SOCK_TYPE via -DSOCK_TYPE=..."
#endif

#ifdef VSOCK_CONNECT
#ifndef PORT
#error "define PORT via -DPORT=..."
#endif
#ifndef CLIENT_PAYLOAD
#error "define CLIENT_PAYLOAD via -DCLIENT_PAYLOAD=..."
#endif
#ifndef EXPECTED_SERVER_PAYLOAD
#error "define EXPECTED_SERVER_PAYLOAD via -DEXPECTED_SERVER_PAYLOAD=..."
#endif
#ifndef CONNECT_DENIED_EXIT_CODE
#error "define CONNECT_DENIED_EXIT_CODE via -DCONNECT_DENIED_EXIT_CODE=..."
#endif

static void timeout(int signal) {
    (void)signal;
    static const char message[] = "socketcall client timed out\n";
    write(STDERR_FILENO, message, sizeof(message) - 1);
    _exit(124);
}

static int write_full(int fd, const char *buf, size_t len) {
    while (len > 0) {
        ssize_t n = write(fd, buf, len);
        if (n < 0 && errno == EINTR)
            continue;
        if (n <= 0) {
            if (n == 0)
                errno = EIO;
            perror("write");
            return -1;
        }
        buf += n;
        len -= (size_t)n;
    }
    return 0;
}

static int read_full(int fd, char *buf, size_t len) {
    while (len > 0) {
        ssize_t n = read(fd, buf, len);
        if (n < 0 && errno == EINTR)
            continue;
        if (n < 0) {
            perror("read");
            return -1;
        }
        if (n == 0) {
            fprintf(stderr, "unexpected EOF while reading server payload\n");
            return -1;
        }
        buf += n;
        len -= (size_t)n;
    }
    return 0;
}
#endif

/*
 * Check that int $0x80 works at all. A kernel built without
 * CONFIG_IA32_EMULATION, or booted with ia32_emulation=0, delivers SIGSEGV
 * instead of returning an error. Run the check in a child process with core
 * dumps disabled, so that the failure is neither fatal nor leaves a core file.
 */
static int ia32_emulation_available(void) {
    pid_t pid = fork();
    if (pid < 0) {
        perror("fork");
        return 1;
    }
    if (pid == 0) {
        struct rlimit no_core = {0, 0};
        setrlimit(RLIMIT_CORE, &no_core);
        int ret;
        asm volatile (
            "int $0x80"
            : "=a"(ret)
            : "a"(SYS_GETPID_I386)
            : "memory"
        );
        _exit(0);
    }

    int status;
    if (waitpid(pid, &status, 0) < 0) {
        perror("waitpid");
        return 1;
    }
    return !(WIFSIGNALED(status) && WTERMSIG(status) == SIGSEGV);
}

int main() {
    if (!ia32_emulation_available()) {
        fprintf(stderr, "ia32 emulation is not available\n");
        return IA32_UNAVAILABLE_EXIT_CODE;
    }

    /*
     * The int $0x80 ia32 compat path truncates all registers to 32 bits.
     * Every pointer used by socketcall must live below 4 GB, so keep its
     * arguments and address in the MAP_32BIT allocation.
     */
    unsigned char *mapping = mmap(NULL, 4096,
        PROT_READ | PROT_WRITE,
        MAP_PRIVATE | MAP_ANONYMOUS | MAP_32BIT,
        -1, 0);
    if (mapping == MAP_FAILED) {
        perror("mmap");
        return 2;
    }
    unsigned int *args = (unsigned int *)mapping;
    args[0] = SOCK_FAMILY;
    args[1] = SOCK_TYPE;
    args[2] = 0;

    int ret;
    asm volatile (
        "int $0x80"
        : "=a"(ret)
        : "a"(SYS_SOCKETCALL_I386), "b"(SYS_SOCKET), "c"(args)
        : "memory"
    );

    if (ret < 0) {
        errno = -ret;
        perror("socket");
        return 1;
    }

#ifdef VSOCK_CONNECT
    signal(SIGALRM, timeout);
    alarm(10);

    int fd = ret;
    struct sockaddr_vm *address = (struct sockaddr_vm *)(mapping + 64);
    memset(address, 0, sizeof(*address));
    address->svm_family = AF_VSOCK;
    address->svm_cid = VMADDR_CID_LOCAL;
    address->svm_port = PORT;

    args[0] = (unsigned int)fd;
    args[1] = (unsigned int)(uintptr_t)address;
    args[2] = sizeof(*address);
    asm volatile (
        "int $0x80"
        : "=a"(ret)
        : "a"(SYS_SOCKETCALL_I386), "b"(SYS_CONNECT), "c"(args)
        : "memory"
    );
    if (ret < 0) {
        int error = -ret;
        errno = error;
        perror("connect");
        close(fd);
        if (error == EPERM || error == EACCES)
            return CONNECT_DENIED_EXIT_CODE;
        return 1;
    }

    static const char client_payload[] = CLIENT_PAYLOAD;
    static const char server_payload[] = EXPECTED_SERVER_PAYLOAD;
    char reply[sizeof(server_payload) - 1];
    if (write_full(fd, client_payload, sizeof(client_payload) - 1) < 0 ||
        read_full(fd, reply, sizeof(reply)) < 0) {
        close(fd);
        return 1;
    }
    if (memcmp(reply, server_payload, sizeof(reply)) != 0) {
        fprintf(stderr, "server payload mismatch\n");
        close(fd);
        return 1;
    }
    alarm(0);
    printf("AF_VSOCK connect via socketcall succeeded\n");
    close(fd);
    return 0;
#else
    printf("socket(%d, %d, 0) via socketcall succeeded\n", SOCK_FAMILY, SOCK_TYPE);
    close(ret);
    return 0;
#endif
}
