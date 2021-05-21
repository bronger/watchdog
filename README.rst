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

  watchdog <directory>

Then, ``<directory>`` and everything beneath is synchronised.


Synchronisation scripts
=======================

Position
--------

We need the following three programs: ``bulk_sync``, ``copy``, and ``delete``.
They must be executables in the same directory as the ``watchdog`` itself.
