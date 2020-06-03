#!/bin/bash
export AISTORE_DIR="$(cd "$(dirname "$0")/../../"; pwd -P)"
export AIS_ENDPOINT=${AIS_ENDPOINT}
export BUCKET=${BUCKET}
YAPF_STYLE="$(dirname ${0})/config/.style.yapf"
PYLINT_STYLE="$(dirname ${0})/config/.pylintrc"
EXTERNAL_SRC_REGEX=".*\(venv\|build\|3rdparty\|dist\|.idea\|.vscode\)/.*"
# This script is used by Makefile to run commands.
source ${AISTORE_DIR}/deploy/scripts/utils.sh

case $1 in
lint)
  echo "Running lint..." >&2
  ${GOPATH}/bin/golangci-lint run $(list_all_go_dirs)
  exit $?
  ;;

fmt)
  err_count=0
  echo "Running style check..." >&2
  case $2 in
  --fix)
    gofmt -w ${AISTORE_DIR}
    python_yapf_fix
    ;;
  *)
    out=$(gofmt -l -e ${AISTORE_DIR})

    if [[ -n ${out} ]]; then
      echo ${out} >&2
      exit 1
    fi

    check_imports
    check_files_headers
    check_python_formatting

    ;;
  esac
  ;;

spell)
  echo "Running spell check..." >&2
  case $2 in
  --fix)
    ${GOPATH}/bin/misspell -w -locale=US ${AISTORE_DIR}
    ;;
  *)
    ${GOPATH}/bin/misspell -error -locale=US ${AISTORE_DIR}
    ;;
  esac
  ;;

test-env)
  if [[ -z ${BUCKET} ]]; then
    echo "Error: missing environment variable: BUCKET=\"bucketname\""
    exit 1
  fi

  hash docker &>/dev/null
  if [[ $? -eq 0 ]]; then
    docker_running=$(docker container ls)
    if [[ $? -ne 0 ]]; then
      echo "Warning: Can't check if AIS is running from docker, verify that you have permissions for /var/run/docker.sock" >&2
    elif [[ $(echo ${docker_running} | grep ais) ]]; then
      echo "AIStore running on docker..." >&2
      exit 0
    fi
  fi
  if [[ -n ${KUBERNETES_SERVICE_HOST} ]]; then
    echo "AIStore running on Kubernetes..." >&2
    if [[ "${AIS_ENDPOINT}" != "" ]]; then
      ip=${AIS_ENDPOINT%:*} # extract IP from format IP:PORT
      if [[ $(ping -c 1 ${ip} | grep '1 received') ]]; then
        echo "AIStore connection to ${ip} is working..." >&2
        exit 0
      else
        echo "Error connecting to ${ip}. Did you specify the correct address?" >&2
        exit 1
      fi
    else
      echo "Error missing environment variable: 'AIS_ENDPOINT=<IP>:<PORT>'" >&2
      exit 1
    fi
  fi
  if [[ $(ps aux | grep -v -e 'grep' | grep bin/aisnode) ]]; then
    echo "AIStore running locally..." >&2
    exit 0
  fi

  echo "AIStore is not running, this causes some tests to fail! (to run, see: https://github.com/NVIDIA/aistore#local-non-containerized)" >&2
  echo -n "continue? [y/N] " >&2 && read ans && [[ ${ans:-N} =~ ^y(es)?$ ]]
  exit $?
  ;;

test-short)
  echo "Running short tests..." >&2
  SECONDS=0
  errs=$(BUCKET=${BUCKET} AIS_ENDPOINT=${AIS_ENDPOINT} go test -v -p 1 -parallel 4 -count 1 -timeout 30m -short "${AISTORE_DIR}/..." 2>&1 | tee -a /dev/stderr | grep -ae "^FAIL\|^--- FAIL")
  err_count=$(echo "${errs}" | wc -l)
  echo "Tests took: $((SECONDS/3600))h$(((SECONDS%3600)/60))m$((SECONDS%60))s"
  if [[ -n ${errs} ]]; then
    echo "${errs}" >&2
    echo "test-short: ${err_count} failed" >&2
    exit 1
  fi
  exit 0
  ;;

test-long)
  echo "Running long tests..." >&2
  SECONDS=0
  errs=$(BUCKET=${BUCKET} AIS_ENDPOINT=${AIS_ENDPOINT} go test -v -p 1 -parallel 4 -count 1 -timeout 2h "${AISTORE_DIR}/..." 2>&1 | tee -a /dev/stderr | grep -ae "^FAIL\|^--- FAIL")
  err_count=$(echo "${errs}" | wc -l)
  echo "Tests took: $((SECONDS/3600))h$(((SECONDS%3600)/60))m$((SECONDS%60))s"
  if [[ -n ${errs} ]]; then
    echo "${errs}" >&2
    echo "test-long: ${err_count} failed" >&2
    exit 1
  fi
  exit 0
  ;;

test-run)
  echo "Running test with regex..." >&2
  errs=$(BUCKET=${BUCKET} AIS_ENDPOINT=${AIS_ENDPOINT} go test -v -p 1 -parallel 4 -count 1 -timeout 2h  -run="${RE}" "${AISTORE_DIR}/..." 2>&1 | tee -a /dev/stderr | grep -ae "^FAIL\|^--- FAIL" )
  err_count=$(echo "${errs}" | wc -l)
  if [[ -n ${errs} ]]; then
    echo "${errs}" >&2
    echo "test-run: ${err_count} failed" >&2
    exit 1
  fi
  exit 0
  ;;

test-docker)
  docker_state=$(docker info >/dev/null 2>&1)
  if [[ $? -ne 0 ]]; then
    echo "Docker does not seem to be running, run it first and retry."
    exit 1
  fi

  echo "Running test in Docker..." >&2
  branch=$(git branch | grep \* | cut -d ' ' -f2)
  errs=$("${AISTORE_DIR}/deploy/test/docker/test.sh" --name=${branch} 2>&1 | tee -a /dev/stderr | grep -e "^FAIL\|^--- FAIL" )
  err_count=$(echo "${errs}" | wc -l)
  if [[ -n ${errs} ]]; then
    echo "${errs}" >&2
    echo "test-run: ${err_count} failed" >&2
    exit 1
  fi
  exit 0
  ;;


test-bench)
  echo "Running benchmark tests..." >&2
  errs=$("${AISTORE_DIR}/deploy/scripts/bench.sh" 2>&1 | tee -a /dev/stderr | grep -e "^--- FAIL: Bench\|^--- FAIL: Test" )
  # TODO: MJ: Replace wherever possible with perror if this works
  perror $1 "${errs}"
  ;;


dev-init)
  if [[ -z ${REMOTE} ]]; then
    echo "Missing environment variable: REMOTE=\"http://path/to/remote\""
    exit
  fi

  if [[ -z $(command -v git) ]]; then
    echo "'git' command not installed"
    exit 1
  elif [[ -z $(git remote -v | grep origin) ]]; then
    git remote add origin "${REMOTE}";
  else
    git remote set-url origin "${REMOTE}";
  fi
  exit 0
  ;;

*)
  echo "unsupported argument $1"
  exit 1
  ;;
esac
