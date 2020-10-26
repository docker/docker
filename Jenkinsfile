#!groovy
pipeline {
    agent none

    options {
        buildDiscarder(logRotator(daysToKeepStr: '30'))
        timeout(time: 2, unit: 'HOURS')
        timestamps()
    }
    parameters {
        booleanParam(name: 'amd64', defaultValue: true, description: 'amd64 (x86_64) Build/Test')
        booleanParam(name: 'rootless', defaultValue: true, description: 'amd64 (x86_64) Build/Test (Rootless mode)')
        booleanParam(name: 'arm64', defaultValue: true, description: 'ARM (arm64) Build/Test')
        booleanParam(name: 's390x', defaultValue: true, description: 'IBM Z (s390x) Build/Test')
        booleanParam(name: 'ppc64le', defaultValue: true, description: 'PowerPC (ppc64le) Build/Test')
        booleanParam(name: 'windowsRS1', defaultValue: false, description: 'Windows 2016 (RS1) Build/Test')
        booleanParam(name: 'windowsRS5', defaultValue: true, description: 'Windows 2019 (RS5) Build/Test')
        booleanParam(name: 'dco', defaultValue: true, description: 'Run the DCO check')
    }
    environment {
        DOCKER_BUILDKIT     = '1'
        DOCKER_EXPERIMENTAL = '1'
        DOCKER_GRAPHDRIVER  = 'overlay2'
        APT_MIRROR          = 'cdn-fastly.deb.debian.org'
        CHECK_CONFIG_COMMIT = '78405559cfe5987174aa2cb6463b9b2c1b917255'
        TESTDEBUG           = '0'
        TIMEOUT             = '120m'
    }
    stages {
        stage('pr-hack') {
            when { changeRequest() }
            steps {
                script {
                    echo "Workaround for PR auto-cancel feature. Borrowed from https://issues.jenkins-ci.org/browse/JENKINS-43353"
                    def buildNumber = env.BUILD_NUMBER as int
                    if (buildNumber > 1) milestone(buildNumber - 1)
                    milestone(buildNumber)
                }
            }
        }
        stage('DCO-check') {
            when {
                beforeAgent true
                expression { params.dco }
            }
            agent { label 'amd64 && ubuntu-1804 && overlay2' }
            steps {
                sh '''
                docker run --rm \
                  -v "$WORKSPACE:/workspace" \
                  -e VALIDATE_REPO=${GIT_URL} \
                  -e VALIDATE_BRANCH=${CHANGE_TARGET} \
                  alpine sh -c 'apk add --no-cache -q bash git openssh-client && cd /workspace && hack/validate/dco'
                '''
            }
        }
        stage('Build') {
            parallel {
                matrix {
                    agent none
                    axes {
                        axis {
                            name 'ARCH_LABEL'
                            values 'amd64 && ubuntu-1804 && overlay2' 's390x-ubuntu-1804' 'ppc64le-ubuntu-1604' 'arm64 && linux'
                        }
                    }
                    stage("${ARCH_LABEL}") {
                        when {
                            beforeAgent true
                            expression { params."${ARCH_LABEL.split(" |-")[0]}" }
                        }
                        agent { label "${ARCH_LABEL}" }

                        stages {
                            stage("Print info") {
                                steps {
                                    sh 'docker version'
                                    sh 'docker info'
                                    sh '''
                                    echo "check-config.sh version: ${CHECK_CONFIG_COMMIT}"
                                    curl -fsSL -o ${WORKSPACE}/check-config.sh "https://raw.githubusercontent.com/moby/moby/${CHECK_CONFIG_COMMIT}/contrib/check-config.sh" \
                                    && bash ${WORKSPACE}/check-config.sh || true
                                    '''
                                }
                            }
                            stage("Build dev image") {
                                steps {
                                    sh '''
                                    docker build --force-rm --build-arg APT_MIRROR -t docker:${GIT_COMMIT} .
                                    '''
                                }
                            }
                            stage("Unit tests") {
                                steps {
                                    sh '''
                                    docker run --rm -t --privileged \
                                    -v "$WORKSPACE/bundles:/go/src/github.com/docker/docker/bundles" \
                                    --name docker-pr$BUILD_NUMBER \
                                    -e DOCKER_EXPERIMENTAL \
                                    -e DOCKER_GITCOMMIT=${GIT_COMMIT} \
                                    -e DOCKER_GRAPHDRIVER \
                                    -e VALIDATE_REPO=${GIT_URL} \
                                    -e VALIDATE_BRANCH=${CHANGE_TARGET} \
                                    docker:${GIT_COMMIT} \
                                    hack/test/unit
                                    '''
                                }
                                post {
                                    always {
                                        junit testResults: 'bundles/junit-report.xml', allowEmptyResults: true
                                    }
                                }
                            }

                            stage("Validate") {
                                when {
                                    expression { ARCH_LABEL.startsWith("amd64") }
                                }

                                steps {
                                    sh '''
                                    docker run --rm -t --privileged \
                                    -v "$WORKSPACE/bundles:/go/src/github.com/docker/docker/bundles" \
                                    -v "$WORKSPACE/.git:/go/src/github.com/docker/docker/.git" \
                                    --name docker-pr$BUILD_NUMBER \
                                    -e DOCKER_EXPERIMENTAL \
                                    -e DOCKER_GITCOMMIT=${GIT_COMMIT} \
                                    -e DOCKER_GRAPHDRIVER \
                                    -e TEST_FORCE_VALIDATE \
                                    -e VALIDATE_REPO=${GIT_URL} \
                                    -e VALIDATE_BRANCH=${CHANGE_TARGET} \
                                    docker:${GIT_COMMIT} \
                                    hack/validate/default
                                    '''
                                }
                            }
                            stage("Docker-py") {
                                when {
                                    expression { ARCH_LABEL.startsWith("amd64") }
                                }
                                steps {
                                    sh '''
                                    docker run --rm -t --privileged \
                                    -v "$WORKSPACE/bundles:/go/src/github.com/docker/docker/bundles" \
                                    --name docker-pr$BUILD_NUMBER \
                                    -e DOCKER_EXPERIMENTAL \
                                    -e DOCKER_GITCOMMIT=${GIT_COMMIT} \
                                    -e DOCKER_GRAPHDRIVER \
                                    -e VALIDATE_REPO=${GIT_URL} \
                                    -e VALIDATE_BRANCH=${CHANGE_TARGET} \
                                    docker:${GIT_COMMIT} \
                                    hack/make.sh \
                                        dynbinary-daemon \
                                        test-docker-py
                                    '''
                                }
                                post {
                                    always {
                                        junit testResults: 'bundles/test-docker-py/junit-report.xml', allowEmptyResults: true

                                        sh '''
                                        echo "Ensuring container killed."
                                        docker rm -vf docker-pr$BUILD_NUMBER || true
                                        '''

                                        sh '''
                                        echo 'Chowning /workspace to jenkins user'
                                        docker run --rm -v "$WORKSPACE:/workspace" busybox chown -R "$(id -u):$(id -g)" /workspace
                                        '''

                                        catchError(buildResult: 'SUCCESS', stageResult: 'FAILURE', message: 'Failed to create bundles.tar.gz') {
                                            sh '''
                                            bundleName=docker-py
                                            echo "Creating ${bundleName}-bundles.tar.gz"
                                            tar -czf ${bundleName}-bundles.tar.gz bundles/test-docker-py/*.xml bundles/test-docker-py/*.log
                                            '''

                                            archiveArtifacts artifacts: '*-bundles.tar.gz', allowEmptyArchive: true
                                        }
                                    }
                                }
                            }
                            stage("Static") {
                                when {
                                    expression { ARCH_LABEL.startsWith("amd64") }
                                }
                                steps {
                                    sh '''
                                    docker run --rm -t --privileged \
                                    -v "$WORKSPACE/bundles:/go/src/github.com/docker/docker/bundles" \
                                    --name docker-pr$BUILD_NUMBER \
                                    -e DOCKER_GITCOMMIT=${GIT_COMMIT} \
                                    -e DOCKER_GRAPHDRIVER \
                                    docker:${GIT_COMMIT} \
                                    hack/make.sh binary-daemon
                                    '''
                                }
                            }
                            stage("Cross") {
                                when {
                                    expression { ARCH_LABEL.startsWith("amd64") }
                                }
                                steps {
                                    sh '''
                                    docker run --rm -t --privileged \
                                    -v "$WORKSPACE/bundles:/go/src/github.com/docker/docker/bundles" \
                                    --name docker-pr$BUILD_NUMBER \
                                    -e DOCKER_GITCOMMIT=${GIT_COMMIT} \
                                    -e DOCKER_GRAPHDRIVER \
                                    docker:${GIT_COMMIT} \
                                    hack/make.sh cross
                                    '''
                                }
                            }

                            stage("Validate vendor") {
                                when {
                                    expression { ARCH_LABEL.startsWith("amd64") }
                                }
                                steps {
                                    sh '''
                                    docker run --rm -t --privileged \
                                    -v "$WORKSPACE/.git:/go/src/github.com/docker/docker/.git" \
                                    --name docker-pr$BUILD_NUMBER \
                                    -e DOCKER_EXPERIMENTAL \
                                    -e DOCKER_GITCOMMIT=${GIT_COMMIT} \
                                    -e DOCKER_GRAPHDRIVER \
                                    -e TEST_FORCE_VALIDATE \
                                    -e VALIDATE_REPO=${GIT_URL} \
                                    -e VALIDATE_BRANCH=${CHANGE_TARGET} \
                                    docker:${GIT_COMMIT} \
                                    hack/validate/vendor
                                    '''
                                }
                            }
                            stage("Integration tests") {
                                environment { TEST_SKIP_INTEGRATION_CLI = '1' }
                                steps {
                                    sh '''
                                    docker run --rm -t --privileged \
                                    -v "$WORKSPACE/bundles:/go/src/github.com/docker/docker/bundles" \
                                    --name docker-pr$BUILD_NUMBER \
                                    -e DOCKER_EXPERIMENTAL \
                                    -e DOCKER_GITCOMMIT=${GIT_COMMIT} \
                                    -e DOCKER_GRAPHDRIVER \
                                    -e TESTDEBUG \
                                    -e TEST_SKIP_INTEGRATION_CLI \
                                    -e TIMEOUT \
                                    -e VALIDATE_REPO=${GIT_URL} \
                                    -e VALIDATE_BRANCH=${CHANGE_TARGET} \
                                    docker:${GIT_COMMIT} \
                                    hack/make.sh \
                                        dynbinary \
                                        test-integration
                                    '''
                                }
                                post {
                                    always {
                                        junit testResults: 'bundles/**/*-report.xml', allowEmptyResults: true
                                    }
                                }
                            }

                            stage("Integration-cli tests") {
                                when {
                                    not { changeRequest() }
                                }
                                environment { TEST_SKIP_INTEGRATION = '1' }
                                steps {
                                    sh '''
                                    docker run --rm -t --privileged \
                                    -v "$WORKSPACE/bundles:/go/src/github.com/docker/docker/bundles" \
                                    --name docker-pr$BUILD_NUMBER \
                                    -e DOCKER_GITCOMMIT=${GIT_COMMIT} \
                                    -e DOCKER_GRAPHDRIVER \
                                    -e TEST_SKIP_INTEGRATION \
                                    -e TIMEOUT \
                                    -e VALIDATE_REPO=${GIT_URL} \
                                    -e VALIDATE_BRANCH=${CHANGE_TARGET} \
                                    docker:${GIT_COMMIT} \
                                    hack/make.sh \
                                        dynbinary \
                                        test-integration
                                    '''
                                }
                                post {
                                    always {
                                        junit testResults: 'bundles/**/*-report.xml', allowEmptyResults: true
                                    }
                                }
                            }

                            stage("Build e2e image") {
                                steps {
                                    sh '''
                                    echo "Building e2e image"
                                    docker build --build-arg DOCKER_GITCOMMIT=${GIT_COMMIT} -t moby-e2e-test -f Dockerfile.e2e .
                                    '''
                                }
                            }
                        }

                        post {
                            always {
                                sh '''
                                echo "Ensuring container killed."
                                docker rm -vf docker-pr$BUILD_NUMBER || true
                                '''

                                sh '''
                                echo "Chowning /workspace to jenkins user"
                                docker run --rm -v "$WORKSPACE:/workspace" busybox chown -R "$(id -u):$(id -g)" /workspace
                                '''

                                catchError(buildResult: 'SUCCESS', stageResult: 'FAILURE', message: 'Failed to create bundles.tar.gz') {
                                    sh '''
                                    bundleName=s390x-integration
                                    echo "Creating ${bundleName}-bundles.tar.gz"
                                    # exclude overlay2 directories
                                    find bundles -path '*/root/*overlay2' -prune -o -type f \\( -name '*-report.json' -o -name '*.log' -o -name '*.prof' -o -name '*-report.xml' \\) -print | xargs tar -czf ${bundleName}-bundles.tar.gz
                                    '''

                                    archiveArtifacts artifacts: '*-bundles.tar.gz', allowEmptyArchive: true
                                }
                            }
                            cleanup {
                                sh 'make clean'
                                deleteDir()
                            }
                        }
                    }
                }
                stage('rootless') {
                    when {
                        beforeAgent true
                        expression { params.rootless }
                    }
                    agent { label 'amd64 && ubuntu-1804 && overlay2' }
                    stages {
                        stage("Print info") {
                            steps {
                                sh 'docker version'
                                sh 'docker info'
                                sh '''
                                echo "check-config.sh version: ${CHECK_CONFIG_COMMIT}"
                                curl -fsSL -o ${WORKSPACE}/check-config.sh "https://raw.githubusercontent.com/moby/moby/${CHECK_CONFIG_COMMIT}/contrib/check-config.sh" \
                                && bash ${WORKSPACE}/check-config.sh || true
                                '''
                            }
                        }
                        stage("Build dev image") {
                            steps {
                                sh '''
                                docker build --force-rm --build-arg APT_MIRROR -t docker:${GIT_COMMIT} .
                                '''
                            }
                        }
                        stage("Integration tests") {
                            environment {
                                DOCKER_ROOTLESS = '1'
                                TEST_SKIP_INTEGRATION_CLI = '1'
                            }
                            steps {
                                sh '''
                                docker run --rm -t --privileged \
                                  -v "$WORKSPACE/bundles:/go/src/github.com/docker/docker/bundles" \
                                  --name docker-pr$BUILD_NUMBER \
                                  -e DOCKER_GITCOMMIT=${GIT_COMMIT} \
                                  -e DOCKER_GRAPHDRIVER \
                                  -e DOCKER_EXPERIMENTAL \
                                  -e DOCKER_ROOTLESS \
                                  -e TEST_SKIP_INTEGRATION_CLI \
                                  -e TIMEOUT \
                                  -e VALIDATE_REPO=${GIT_URL} \
                                  -e VALIDATE_BRANCH=${CHANGE_TARGET} \
                                  docker:${GIT_COMMIT} \
                                  hack/make.sh \
                                    dynbinary \
                                    test-integration
                                '''
                            }
                            post {
                                always {
                                    junit testResults: 'bundles/**/*-report.xml', allowEmptyResults: true
                                }
                            }
                        }
                    }

                    post {
                        always {
                            sh '''
                            echo "Ensuring container killed."
                            docker rm -vf docker-pr$BUILD_NUMBER || true
                            '''

                            sh '''
                            echo "Chowning /workspace to jenkins user"
                            docker run --rm -v "$WORKSPACE:/workspace" busybox chown -R "$(id -u):$(id -g)" /workspace
                            '''

                            catchError(buildResult: 'SUCCESS', stageResult: 'FAILURE', message: 'Failed to create bundles.tar.gz') {
                                sh '''
                                bundleName=amd64-rootless
                                echo "Creating ${bundleName}-bundles.tar.gz"
                                # exclude overlay2 directories
                                find bundles -path '*/root/*overlay2' -prune -o -type f \\( -name '*-report.json' -o -name '*.log' -o -name '*.prof' -o -name '*-report.xml' \\) -print | xargs tar -czf ${bundleName}-bundles.tar.gz
                                '''

                                archiveArtifacts artifacts: '*-bundles.tar.gz', allowEmptyArchive: true
                            }
                        }
                        cleanup {
                            sh 'make clean'
                            deleteDir()
                        }
                    }
                }
                stage('win-RS1') {
                    when {
                        beforeAgent true
                        // Skip this stage on PRs unless the windowsRS1 checkbox is selected
                        anyOf {
                            not { changeRequest() }
                            expression { params.windowsRS1 }
                        }
                    }
                    environment {
                        DOCKER_BUILDKIT        = '0'
                        DOCKER_DUT_DEBUG       = '1'
                        SKIP_VALIDATION_TESTS  = '1'
                        SOURCES_DRIVE          = 'd'
                        SOURCES_SUBDIR         = 'gopath'
                        TESTRUN_DRIVE          = 'd'
                        TESTRUN_SUBDIR         = "CI"
                        WINDOWS_BASE_IMAGE     = 'mcr.microsoft.com/windows/servercore'
                        WINDOWS_BASE_IMAGE_TAG = 'ltsc2016'
                    }
                    agent {
                        node {
                            customWorkspace 'd:\\gopath\\src\\github.com\\docker\\docker'
                            label 'windows-2016'
                        }
                    }
                    stages {
                        stage("Print info") {
                            steps {
                                sh 'docker version'
                                sh 'docker info'
                            }
                        }
                        stage("Run tests") {
                            steps {
                                powershell '''
                                $ErrorActionPreference = 'Stop'
                                [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
                                Invoke-WebRequest https://github.com/moby/docker-ci-zap/blob/master/docker-ci-zap.exe?raw=true -OutFile C:/Windows/System32/docker-ci-zap.exe
                                ./hack/ci/windows.ps1
                                exit $LastExitCode
                                '''
                            }
                        }
                    }
                    post {
                        always {
                            junit testResults: 'bundles/junit-report-*.xml', allowEmptyResults: true
                            catchError(buildResult: 'SUCCESS', stageResult: 'FAILURE', message: 'Failed to create bundles.tar.gz') {
                                powershell '''
                                cd $env:WORKSPACE
                                $bundleName="windowsRS1-integration"
                                Write-Host -ForegroundColor Green "Creating ${bundleName}-bundles.zip"

                                # archiveArtifacts does not support env-vars to , so save the artifacts in a fixed location
                                Compress-Archive -Path "bundles/CIDUT.out", "bundles/CIDUT.err", "bundles/junit-report-*.xml" -CompressionLevel Optimal -DestinationPath "${bundleName}-bundles.zip"
                                '''

                                archiveArtifacts artifacts: '*-bundles.zip', allowEmptyArchive: true
                            }
                        }
                        cleanup {
                            sh 'make clean'
                            deleteDir()
                        }
                    }
                }
                stage('win-RS5') {
                    when {
                        beforeAgent true
                        expression { params.windowsRS5 }
                    }
                    environment {
                        DOCKER_BUILDKIT        = '0'
                        DOCKER_DUT_DEBUG       = '1'
                        SKIP_VALIDATION_TESTS  = '1'
                        SOURCES_DRIVE          = 'd'
                        SOURCES_SUBDIR         = 'gopath'
                        TESTRUN_DRIVE          = 'd'
                        TESTRUN_SUBDIR         = "CI"
                        WINDOWS_BASE_IMAGE     = 'mcr.microsoft.com/windows/servercore'
                        WINDOWS_BASE_IMAGE_TAG = 'ltsc2019'
                    }
                    agent {
                        node {
                            customWorkspace 'd:\\gopath\\src\\github.com\\docker\\docker'
                            label 'windows-2019'
                        }
                    }
                    stages {
                        stage("Print info") {
                            steps {
                                sh 'docker version'
                                sh 'docker info'
                            }
                        }
                        stage("Run tests") {
                            steps {
                                powershell '''
                                $ErrorActionPreference = 'Stop'
                                Invoke-WebRequest https://github.com/moby/docker-ci-zap/blob/master/docker-ci-zap.exe?raw=true -OutFile C:/Windows/System32/docker-ci-zap.exe
                                ./hack/ci/windows.ps1
                                exit $LastExitCode
                                '''
                            }
                        }
                    }
                    post {
                        always {
                            junit testResults: 'bundles/junit-report-*.xml', allowEmptyResults: true
                            catchError(buildResult: 'SUCCESS', stageResult: 'FAILURE', message: 'Failed to create bundles.tar.gz') {
                                powershell '''
                                cd $env:WORKSPACE
                                $bundleName="windowsRS5-integration"
                                Write-Host -ForegroundColor Green "Creating ${bundleName}-bundles.zip"

                                # archiveArtifacts does not support env-vars to , so save the artifacts in a fixed location
                                Compress-Archive -Path "bundles/CIDUT.out", "bundles/CIDUT.err", "bundles/junit-report-*.xml" -CompressionLevel Optimal -DestinationPath "${bundleName}-bundles.zip"
                                '''

                                archiveArtifacts artifacts: '*-bundles.zip', allowEmptyArchive: true
                            }
                        }
                        cleanup {
                            sh 'make clean'
                            deleteDir()
                        }
                    }
                }
            }
        }
    }
}
