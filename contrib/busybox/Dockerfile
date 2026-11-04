# Source: https://frippery.org/busybox/
# This Dockerfile builds a (64-bit) busybox images which is suitable for
# running many of the integration-cli tests for Docker against a Windows
# daemon.
#
# Based on https://github.com/jhowardmsft/busybox
# John Howard (IRC jhowardmsft, Email john.howard@microsoft.com)
#
# To build: docker build -t busybox .
# To publish: Needs someone with publishing rights
ARG WINDOWS_BASE_IMAGE=mcr.microsoft.com/windows/nanoserver
ARG WINDOWS_BASE_IMAGE_TAG=ltsc2022
ARG POWERSHELL_VERSION=7.6.2
ARG POWERSHELL_SHA256SUM=32e0dd26752483ba3f0e40e9ae44150643cbff469c13210c93295d158bfd7b26
ARG BUSYBOX_VERSION=FRP-5007-g82accfc19

# Checksum taken from https://frippery.org/files/busybox/SHA256SUM
ARG BUSYBOX_SHA256SUM=8263abc2aee6b6f3b3c41aa704bee98987bf7b1ea9bce833f75c7bce89ec1602

FROM ${WINDOWS_BASE_IMAGE}:${WINDOWS_BASE_IMAGE_TAG}
RUN mkdir C:\tmp && mkdir C:\bin && mkdir C:\PowerShell
ARG POWERSHELL_VERSION
ARG POWERSHELL_SHA256SUM
ARG BUSYBOX_VERSION
ARG BUSYBOX_SHA256SUM
ADD https://github.com/moby/busybox/releases/download/${BUSYBOX_VERSION}/busybox-w64-${BUSYBOX_VERSION}.exe /bin/busybox.exe

RUN curl.exe -fSLo C:\PowerShell.zip https://github.com/PowerShell/PowerShell/releases/download/v%POWERSHELL_VERSION%/PowerShell-%POWERSHELL_VERSION%-win-x64.zip && \
    tar -xf C:\PowerShell.zip -C C:\PowerShell && \
    C:\PowerShell\pwsh.exe -NoLogo -NoProfile -Command "if ((Get-FileHash -Path C:\PowerShell.zip -Algorithm SHA256).Hash -ne $Env:POWERSHELL_SHA256SUM) { Throw 'PowerShell checksum validation failed' }" && \
    del C:\PowerShell.zip
RUN setx /M PATH "C:\PowerShell;C:\bin;%PATH%"
RUN pwsh -NoLogo -NoProfile -Command \
    if ((Get-FileHash -Path /bin/busybox.exe -Algorithm SHA256).Hash -ne $Env:BUSYBOX_SHA256SUM) { \
        Throw \"Checksum validation failed\" \
    }

RUN pwsh -NoLogo -NoProfile -Command busybox.exe --list ^|%{$nul = cmd /c mklink C:\bin\$_.exe busybox.exe}
CMD ["sh"]
