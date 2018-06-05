// This is a duplicated and slightly modified version of "gopkg.in/karalabe/cookiejar.v2/collections/prque".

package prque

// The size of a block of data
const blockSize = 4096

// compareFn returns true if a comes before b
type compareFn func(a, b interface{}) bool

// setIndexCallback is called when the element is moved to a new index.
// Providing setIndexCallback is optional, it is needed only if the application needs
// to delete elements other than the top one.
type setIndexCallback func(a interface{}, i int)

// Internal sortable stack data structure. Implements the Push and Pop ops for
// the stack (heap) functionality and the Len, Less and Swap methods for the
// sortability requirements of the heaps.
type sstack struct {
	compare  compareFn
	setIndex setIndexCallback
	size     int
	capacity int
	offset   int

	blocks [][]interface{}
	active []interface{}
}

// Creates a new, empty stack.
func newSstack(compare compareFn, setIndex setIndexCallback) *sstack {
	result := new(sstack)
	result.compare = compare
	result.setIndex = setIndex
	result.active = make([]interface{}, blockSize)
	result.blocks = [][]interface{}{result.active}
	result.capacity = blockSize
	return result
}

// Pushes a value onto the stack, expanding it if necessary. Required by
// heap.Interface.
func (s *sstack) Push(data interface{}) {
	if s.size == s.capacity {
		s.active = make([]interface{}, blockSize)
		s.blocks = append(s.blocks, s.active)
		s.capacity += blockSize
		s.offset = 0
	} else if s.offset == blockSize {
		s.active = s.blocks[s.size/blockSize]
		s.offset = 0
	}
	if s.setIndex != nil {
		s.setIndex(data, s.size)
	}
	s.active[s.offset] = data
	s.offset++
	s.size++
}

// Pops a value off the stack and returns it. Currently no shrinking is done.
// Required by heap.Interface.
func (s *sstack) Pop() (res interface{}) {
	s.size--
	s.offset--
	if s.offset < 0 {
		s.offset = blockSize - 1
		s.active = s.blocks[s.size/blockSize]
	}
	res, s.active[s.offset] = s.active[s.offset], nil
	if s.setIndex != nil {
		s.setIndex(res, -1)
	}
	return
}

// Returns the length of the stack. Required by sort.Interface.
func (s *sstack) Len() int {
	return s.size
}

// Compares the priority of two elements of the stack (higher is first).
// Required by sort.Interface.
func (s *sstack) Less(i, j int) bool {
	return s.compare(s.blocks[i/blockSize][i%blockSize], s.blocks[j/blockSize][j%blockSize])
}

// Swaps two elements in the stack. Required by sort.Interface.
func (s *sstack) Swap(i, j int) {
	ib, io, jb, jo := i/blockSize, i%blockSize, j/blockSize, j%blockSize
	a, b := s.blocks[jb][jo], s.blocks[ib][io]
	if s.setIndex != nil {
		s.setIndex(a, i)
		s.setIndex(b, j)
	}
	s.blocks[ib][io], s.blocks[jb][jo] = a, b
}

// Resets the stack, effectively clearing its contents.
func (s *sstack) Reset() {
	*s = *newSstack(s.compare, s.setIndex)
}
