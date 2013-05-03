DVID: Distributed, Versioned Image Datastore
====

*Status: In development, not ready for use.*

DVID is a distributed, versioned image datastore that uses leveldb for data storage and a Go language layer that provides http and command-line access.

Documentation is [available here](http://godoc.org/github.com/janelia-flyem/dvid).

## Build Process

DVID uses the [buildem system](http://github.com/janelia-flyem/buildem#readme) to 
automatically download and build leveldb, Go language support, and all required Go packages.  

To build DVID using buildem, do the following steps:

    % cd /path/to/dvid/dir
    % mkdir build
    % cmake -D BUILDEM_DIR=/path/to/buildem/dir ..

If you haven't built with that buildem directory before, do the additional steps:

    % make
    % cmake -D BUILDEM_DIR=/path/to/buildem/dir ..

To build DVID, assuming you are still in the CMake build directory from above:

    % make dvid

This will install a DVID executable 'dvid' in the buildem bin directory.

To build DVID executable without built-in web client:

    % make dvid-exe

DVID can be built manually, without buildem, by the following steps:

1. Build shared leveldb libraries from [Google's repo](https://code.google.com/p/leveldb/).
2. Add the following Go packages using "go get":

    go get code.google.com/p/snappy-go/snappy

    go get bitbucket.org/tebeka/nrsc

    go get code.google.com/p/go-uuid/uuid

    go get github.com/jmhodges/levigo
    