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

  watchdog <configuration-directory>


Configuration file
==================

The configuration directory must contain a file ``confguration.yaml`` which may
look like this:

.. code-block:: yaml

  current dir: /home/bronger
  watched dirs:
    - root: Mail
      gathering ms: 100
      excludes:
        - ^Mail/\.#active
        - ^Mail/active
        - /\.overview
        - /\.#\.overview

``current dir`` should be an absolute path.  Each ``root`` is relative to
``current dir``.  The ``excludes`` are Go-style (non-POSIX) regular expressions.

``gathering ms`` is 10 by default and denotes the milliseconds to wait after a
change for further changes.  Those are then processed with a minimal number of
calls of the synchronisation scripts.


Synchronisation scripts
=======================

Position
--------

We need the following three programs: ``bulk_sync``, ``copy``, and ``delete``.
They must be executables in ``<configuration-directory>``.  There are example
scripts in this repository.

The single argument passed to the scripts is relative to ``current dir`` from
the configuration.


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

This deletes its argument – which may be a file or a directory – from the remote.
