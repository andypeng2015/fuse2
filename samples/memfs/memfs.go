// Copyright 2015 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package memfs

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/googlecloudplatform/gcsfuse/timeutil"
	"github.com/jacobsa/fuse"
	"github.com/jacobsa/fuse/fuseops"
	"github.com/jacobsa/fuse/fuseutil"
	"github.com/jacobsa/gcloud/syncutil"
)

type memFS struct {
	fuseutil.NotImplementedFileSystem

	/////////////////////////
	// Dependencies
	/////////////////////////

	clock timeutil.Clock

	/////////////////////////
	// Mutable state
	/////////////////////////

	// When acquiring this lock, the caller must hold no inode locks.
	mu syncutil.InvariantMutex

	// The collection of live inodes, indexed by ID. IDs of free inodes that may
	// be re-used have nil entries. No ID less than fuseops.RootInodeID is ever
	// used.
	//
	// INVARIANT: len(inodes) > fuseops.RootInodeID
	// INVARIANT: For all i < fuseops.RootInodeID, inodes[i] == nil
	// INVARIANT: inodes[fuseops.RootInodeID] != nil
	// INVARIANT: inodes[fuseops.RootInodeID].isDir()
	inodes []*inode // GUARDED_BY(mu)

	// A list of inode IDs within inodes available for reuse, not including the
	// reserved IDs less than fuseops.RootInodeID.
	//
	// INVARIANT: This is all and only indices i of 'inodes' such that i >
	// fuseops.RootInodeID and inodes[i] == nil
	freeInodes []fuseops.InodeID // GUARDED_BY(mu)
}

// Create a file system that stores data and metadata in memory.
//
// The supplied UID/GID pair will own the root inode. This file system does no
// permissions checking, and should therefore be mounted with the
// default_permissions option.
func NewMemFS(
	uid uint32,
	gid uint32,
	clock timeutil.Clock) fuse.Server {
	// Set up the basic struct.
	fs := &memFS{
		clock:  clock,
		inodes: make([]*inode, fuseops.RootInodeID+1),
	}

	// Set up the root inode.
	rootAttrs := fuseops.InodeAttributes{
		Mode: 0700 | os.ModeDir,
		Uid:  uid,
		Gid:  gid,
	}

	fs.inodes[fuseops.RootInodeID] = newInode(clock, rootAttrs)

	// Set up invariant checking.
	fs.mu = syncutil.NewInvariantMutex(fs.checkInvariants)

	return fuseutil.NewFileSystemServer(fs)
}

////////////////////////////////////////////////////////////////////////
// Helpers
////////////////////////////////////////////////////////////////////////

func (fs *memFS) checkInvariants() {
	// Check reserved inodes.
	for i := 0; i < fuseops.RootInodeID; i++ {
		if fs.inodes[i] != nil {
			panic(fmt.Sprintf("Non-nil inode for ID: %v", i))
		}
	}

	// Check the root inode.
	if !fs.inodes[fuseops.RootInodeID].isDir() {
		panic("Expected root to be a directory.")
	}

	// Build our own list of free IDs.
	freeIDsEncountered := make(map[fuseops.InodeID]struct{})
	for i := fuseops.RootInodeID + 1; i < len(fs.inodes); i++ {
		inode := fs.inodes[i]
		if inode == nil {
			freeIDsEncountered[fuseops.InodeID(i)] = struct{}{}
			continue
		}
	}

	// Check fs.freeInodes.
	if len(fs.freeInodes) != len(freeIDsEncountered) {
		panic(
			fmt.Sprintf(
				"Length mismatch: %v vs. %v",
				len(fs.freeInodes),
				len(freeIDsEncountered)))
	}

	for _, id := range fs.freeInodes {
		if _, ok := freeIDsEncountered[id]; !ok {
			panic(fmt.Sprintf("Unexected free inode ID: %v", id))
		}
	}
}

// Find the given inode and return it with its lock held. Panic if it doesn't
// exist.
//
// SHARED_LOCKS_REQUIRED(fs.mu)
// EXCLUSIVE_LOCK_FUNCTION(inode.mu)
func (fs *memFS) getInodeForModifyingOrDie(id fuseops.InodeID) (inode *inode) {
	inode = fs.inodes[id]
	if inode == nil {
		panic(fmt.Sprintf("Unknown inode: %v", id))
	}

	inode.mu.Lock()
	return
}

// Find the given inode and return it with its lock held for reading. Panic if
// it doesn't exist.
//
// SHARED_LOCKS_REQUIRED(fs.mu)
// SHARED_LOCK_FUNCTION(inode.mu)
func (fs *memFS) getInodeForReadingOrDie(id fuseops.InodeID) (inode *inode) {
	inode = fs.inodes[id]
	if inode == nil {
		panic(fmt.Sprintf("Unknown inode: %v", id))
	}

	inode.mu.Lock()
	return
}

// Allocate a new inode, assigning it an ID that is not in use. Return it with
// its lock held.
//
// EXCLUSIVE_LOCKS_REQUIRED(fs.mu)
// EXCLUSIVE_LOCK_FUNCTION(inode.mu)
func (fs *memFS) allocateInode(
	attrs fuseops.InodeAttributes) (id fuseops.InodeID, inode *inode) {
	// Create and lock the inode.
	inode = newInode(fs.clock, attrs)
	inode.mu.Lock()

	// Re-use a free ID if possible. Otherwise mint a new one.
	numFree := len(fs.freeInodes)
	if numFree != 0 {
		id = fs.freeInodes[numFree-1]
		fs.freeInodes = fs.freeInodes[:numFree-1]
		fs.inodes[id] = inode
	} else {
		id = fuseops.InodeID(len(fs.inodes))
		fs.inodes = append(fs.inodes, inode)
	}

	return
}

// EXCLUSIVE_LOCKS_REQUIRED(fs.mu)
func (fs *memFS) deallocateInode(id fuseops.InodeID) {
	fs.freeInodes = append(fs.freeInodes, id)
	fs.inodes[id] = nil
}

////////////////////////////////////////////////////////////////////////
// FileSystem methods
////////////////////////////////////////////////////////////////////////

func (fs *memFS) Init(
	op *fuseops.InitOp) {
	var err error
	defer fuseutil.RespondToOp(op, &err)

	return
}

func (fs *memFS) LookUpInode(
	op *fuseops.LookUpInodeOp) {
	var err error
	defer fuseutil.RespondToOp(op, &err)

	fs.mu.Lock()
	defer fs.mu.Unlock()

	// Grab the parent directory.
	inode := fs.getInodeForReadingOrDie(op.Parent)
	defer inode.mu.Unlock()

	// Does the directory have an entry with the given name?
	childID, ok := inode.LookUpChild(op.Name)
	if !ok {
		err = fuse.ENOENT
		return
	}

	// Grab the child.
	child := fs.getInodeForReadingOrDie(childID)
	defer child.mu.Unlock()

	// Fill in the response.
	op.Entry.Child = childID
	op.Entry.Attributes = child.attrs

	// We don't spontaneously mutate, so the kernel can cache as long as it wants
	// (since it also handles invalidation).
	op.Entry.AttributesExpiration = fs.clock.Now().Add(365 * 24 * time.Hour)
	op.Entry.EntryExpiration = op.Entry.EntryExpiration

	return
}

func (fs *memFS) GetInodeAttributes(
	op *fuseops.GetInodeAttributesOp) {
	var err error
	defer fuseutil.RespondToOp(op, &err)

	fs.mu.Lock()
	defer fs.mu.Unlock()

	// Grab the inode.
	inode := fs.getInodeForReadingOrDie(op.Inode)
	defer inode.mu.Unlock()

	// Fill in the response.
	op.Attributes = inode.attrs

	// We don't spontaneously mutate, so the kernel can cache as long as it wants
	// (since it also handles invalidation).
	op.AttributesExpiration = fs.clock.Now().Add(365 * 24 * time.Hour)

	return
}

func (fs *memFS) SetInodeAttributes(
	op *fuseops.SetInodeAttributesOp) {
	var err error
	defer fuseutil.RespondToOp(op, &err)

	fs.mu.Lock()
	defer fs.mu.Unlock()

	// Grab the inode.
	inode := fs.getInodeForModifyingOrDie(op.Inode)
	defer inode.mu.Unlock()

	// Handle the request.
	inode.SetAttributes(op.Size, op.Mode, op.Mtime)

	// Fill in the response.
	op.Attributes = inode.attrs

	// We don't spontaneously mutate, so the kernel can cache as long as it wants
	// (since it also handles invalidation).
	op.AttributesExpiration = fs.clock.Now().Add(365 * 24 * time.Hour)

	return
}

func (fs *memFS) MkDir(
	op *fuseops.MkDirOp) {
	var err error
	defer fuseutil.RespondToOp(op, &err)

	fs.mu.Lock()
	defer fs.mu.Unlock()

	// Grab the parent, which we will update shortly.
	parent := fs.getInodeForModifyingOrDie(op.Parent)
	defer parent.mu.Unlock()

	// Set up attributes from the child, using the credentials of the calling
	// process as owner (matching inode_init_owner, cf. http://goo.gl/5qavg8).
	childAttrs := fuseops.InodeAttributes{
		Nlink: 1,
		Mode:  op.Mode,
		Uid:   op.Header().Uid,
		Gid:   op.Header().Gid,
	}

	// Allocate a child.
	childID, child := fs.allocateInode(childAttrs)
	defer child.mu.Unlock()

	// Add an entry in the parent.
	parent.AddChild(childID, op.Name, fuseutil.DT_Directory)

	// Fill in the response.
	op.Entry.Child = childID
	op.Entry.Attributes = child.attrs

	// We don't spontaneously mutate, so the kernel can cache as long as it wants
	// (since it also handles invalidation).
	op.Entry.AttributesExpiration = fs.clock.Now().Add(365 * 24 * time.Hour)
	op.Entry.EntryExpiration = op.Entry.EntryExpiration

	return
}

func (fs *memFS) CreateFile(
	op *fuseops.CreateFileOp) {
	var err error
	defer fuseutil.RespondToOp(op, &err)

	fs.mu.Lock()
	defer fs.mu.Unlock()

	// Grab the parent, which we will update shortly.
	parent := fs.getInodeForModifyingOrDie(op.Parent)
	defer parent.mu.Unlock()

	// Ensure that the name doesn't alread exist, so we don't wind up with a
	// duplicate.
	_, exists := parent.LookUpChild(op.Name)
	if exists {
		err = fuse.EEXIST
		return
	}

	// Set up attributes from the child, using the credentials of the calling
	// process as owner (matching inode_init_owner, cf. http://goo.gl/5qavg8).
	now := fs.clock.Now()
	childAttrs := fuseops.InodeAttributes{
		Nlink:  1,
		Mode:   op.Mode,
		Atime:  now,
		Mtime:  now,
		Ctime:  now,
		Crtime: now,
		Uid:    op.Header().Uid,
		Gid:    op.Header().Gid,
	}

	// Allocate a child.
	childID, child := fs.allocateInode(childAttrs)
	defer child.mu.Unlock()

	// Add an entry in the parent.
	parent.AddChild(childID, op.Name, fuseutil.DT_File)

	// Fill in the response entry.
	op.Entry.Child = childID
	op.Entry.Attributes = child.attrs

	// We don't spontaneously mutate, so the kernel can cache as long as it wants
	// (since it also handles invalidation).
	op.Entry.AttributesExpiration = fs.clock.Now().Add(365 * 24 * time.Hour)
	op.Entry.EntryExpiration = op.Entry.EntryExpiration

	// We have nothing interesting to put in the Handle field.

	return
}

func (fs *memFS) CreateSymlink(
	op *fuseops.CreateSymlinkOp) {
	var err error
	defer fuseutil.RespondToOp(op, &err)

	fs.mu.Lock()
	defer fs.mu.Unlock()

	// Grab the parent, which we will update shortly.
	parent := fs.getInodeForModifyingOrDie(op.Parent)
	defer parent.mu.Unlock()

	// Set up attributes from the child, using the credentials of the calling
	// process as owner (matching inode_init_owner, cf. http://goo.gl/5qavg8).
	now := fs.clock.Now()
	childAttrs := fuseops.InodeAttributes{
		Nlink:  1,
		Mode:   0444 | os.ModeSymlink,
		Atime:  now,
		Mtime:  now,
		Ctime:  now,
		Crtime: now,
		Uid:    op.Header().Uid,
		Gid:    op.Header().Gid,
	}

	// Allocate a child.
	childID, child := fs.allocateInode(childAttrs)
	defer child.mu.Unlock()

	// Set up its target.
	child.target = op.Target

	// Add an entry in the parent.
	parent.AddChild(childID, op.Name, fuseutil.DT_Link)

	// Fill in the response entry.
	op.Entry.Child = childID
	op.Entry.Attributes = child.attrs

	// We don't spontaneously mutate, so the kernel can cache as long as it wants
	// (since it also handles invalidation).
	op.Entry.AttributesExpiration = fs.clock.Now().Add(365 * 24 * time.Hour)
	op.Entry.EntryExpiration = op.Entry.EntryExpiration

	return
}

func (fs *memFS) RmDir(
	op *fuseops.RmDirOp) {
	var err error
	defer fuseutil.RespondToOp(op, &err)

	fs.mu.Lock()
	defer fs.mu.Unlock()

	// Grab the parent, which we will update shortly.
	parent := fs.getInodeForModifyingOrDie(op.Parent)
	defer parent.mu.Unlock()

	// Find the child within the parent.
	childID, ok := parent.LookUpChild(op.Name)
	if !ok {
		err = fuse.ENOENT
		return
	}

	// Grab the child.
	child := fs.getInodeForModifyingOrDie(childID)
	defer child.mu.Unlock()

	// Make sure the child is empty.
	if child.Len() != 0 {
		err = fuse.ENOTEMPTY
		return
	}

	// Remove the entry within the parent.
	parent.RemoveChild(op.Name)

	// Mark the child as unlinked.
	child.attrs.Nlink--

	return
}

func (fs *memFS) Unlink(
	op *fuseops.UnlinkOp) {
	var err error
	defer fuseutil.RespondToOp(op, &err)

	fs.mu.Lock()
	defer fs.mu.Unlock()

	// Grab the parent, which we will update shortly.
	parent := fs.getInodeForModifyingOrDie(op.Parent)
	defer parent.mu.Unlock()

	// Find the child within the parent.
	childID, ok := parent.LookUpChild(op.Name)
	if !ok {
		err = fuse.ENOENT
		return
	}

	// Grab the child.
	child := fs.getInodeForModifyingOrDie(childID)
	defer child.mu.Unlock()

	// Remove the entry within the parent.
	parent.RemoveChild(op.Name)

	// Mark the child as unlinked.
	child.attrs.Nlink--

	return
}

func (fs *memFS) OpenDir(
	op *fuseops.OpenDirOp) {
	var err error
	defer fuseutil.RespondToOp(op, &err)

	fs.mu.Lock()
	defer fs.mu.Unlock()

	// We don't mutate spontaneosuly, so if the VFS layer has asked for an
	// inode that doesn't exist, something screwed up earlier (a lookup, a
	// cache invalidation, etc.).
	inode := fs.getInodeForReadingOrDie(op.Inode)
	defer inode.mu.Unlock()

	if !inode.isDir() {
		panic("Found non-dir.")
	}

	return
}

func (fs *memFS) ReadDir(
	op *fuseops.ReadDirOp) {
	var err error
	defer fuseutil.RespondToOp(op, &err)

	fs.mu.Lock()
	defer fs.mu.Unlock()

	// Grab the directory.
	inode := fs.getInodeForReadingOrDie(op.Inode)
	defer inode.mu.Unlock()

	// Serve the request.
	op.Data, err = inode.ReadDir(int(op.Offset), op.Size)
	if err != nil {
		err = fmt.Errorf("inode.ReadDir: %v", err)
		return
	}

	return
}

func (fs *memFS) OpenFile(
	op *fuseops.OpenFileOp) {
	var err error
	defer fuseutil.RespondToOp(op, &err)

	fs.mu.Lock()
	defer fs.mu.Unlock()

	// We don't mutate spontaneosuly, so if the VFS layer has asked for an
	// inode that doesn't exist, something screwed up earlier (a lookup, a
	// cache invalidation, etc.).
	inode := fs.getInodeForReadingOrDie(op.Inode)
	defer inode.mu.Unlock()

	if !inode.isFile() {
		panic("Found non-file.")
	}

	return
}

func (fs *memFS) ReadFile(
	op *fuseops.ReadFileOp) {
	var err error
	defer fuseutil.RespondToOp(op, &err)

	fs.mu.Lock()
	defer fs.mu.Unlock()

	// Find the inode in question.
	inode := fs.getInodeForReadingOrDie(op.Inode)
	defer inode.mu.Unlock()

	// Serve the request.
	op.Data = make([]byte, op.Size)
	n, err := inode.ReadAt(op.Data, op.Offset)
	op.Data = op.Data[:n]

	// Don't return EOF errors; we just indicate EOF to fuse using a short read.
	if err == io.EOF {
		err = nil
	}

	return
}

func (fs *memFS) WriteFile(
	op *fuseops.WriteFileOp) {
	var err error
	defer fuseutil.RespondToOp(op, &err)

	fs.mu.Lock()
	defer fs.mu.Unlock()

	// Find the inode in question.
	inode := fs.getInodeForModifyingOrDie(op.Inode)
	defer inode.mu.Unlock()

	// Serve the request.
	_, err = inode.WriteAt(op.Data, op.Offset)

	return
}

func (fs *memFS) ReadSymlink(
	op *fuseops.ReadSymlinkOp) {
	var err error
	defer fuseutil.RespondToOp(op, &err)

	fs.mu.Lock()
	defer fs.mu.Unlock()

	// Find the inode in question.
	inode := fs.getInodeForReadingOrDie(op.Inode)
	defer inode.mu.Unlock()

	// Serve the request.
	op.Target = inode.target

	return
}
