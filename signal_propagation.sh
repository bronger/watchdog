# Inspired by <https://unix.stackexchange.com/a/444676/78728>.
#
# Makes a my_long_running_process interruptable by a SIGINT or SIGTERM which
# the shell receive.  In this case, TERM is sent to the child process.  (This
# can be changed, see below.)
#
# Note that this only works for child processes which never return exit codes
# 130 or 143, unless they received SIGINT or SIGTERM, respectively.  If they
# do, you must wrap them in a subshell.
#
# Usage:
#
# prep_term
# my_long_running_process &
# wait_term
# echo $?
#
# You may pass a signal name to “prep_term”.  Default is “TERM”.


prep_term()
{
    unset term_child_pid
    unset term_kill_needed
    signal_name=${1:-TERM}
    trap 'handle_term' TERM INT
}

handle_term()
{
    if [ "${term_child_pid}" ]
    then
        kill -$signal_name "${term_child_pid}" 2>/dev/null
    else
        term_kill_needed="yes"
    fi
}

wait_term()
{
    term_child_pid=$!
    if [ "${term_kill_needed}" ]
    then
        kill -$signal_name "${term_child_pid}" 2>/dev/null
    fi
    wait ${term_child_pid}
    exit_code=$?
    trap - TERM INT
    if [ $exit_code -eq 143 -o $exit_code -eq 130 ]
    then
        wait ${term_child_pid}
        exit_code=$?
    fi
    return $exit_code
}
