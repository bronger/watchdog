Purpose
=======

The watchdog watches changes in a directory recursively, and synchronises these
changes with remote sites.


Alternatives
============

sshfs
  This has possibly slow operations, shadows the local directory structure, and
  cannot synchronise with more than one remote.

inotifywait + rsync
  This is slow with large directories


Call signature
==============

You call the watchdog with::

  watchdog <scripts-directory> <watch-directory>

Then, ``<watch-directory>`` and everything beneath is synchronised.


Synchronisation scripts
=======================

Position
--------

We need the following three programs: ``bulk_sync``, ``copy``, and ``delete``.
They must be executables in ``<scripts-directory>``.  There are example scripts
in this repository.

The single argument passed to the scripts is relative to the path from which
the watchdog was called.


``bulk_rsync``
--------------

This synchronises its argument – which may be a file or a directory – with the
remote.  It must make sure that files and direcories on the remote missing
locally are deleted remotely.


``copy``
--------

This copy its argument – a file – to the remote.


``delete``
----------

This deletes its argument – a file – from the remote.
