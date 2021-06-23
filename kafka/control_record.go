//nolint
package kafka

// The types on this file have been copied from https://github.com/Shopify/sarama and are used for decoding requests.
// As the decoder/encoder interfaces in Sarama project are not public, there is no way of reusing.
// The following issue has been opened https://github.com/Shopify/sarama/issues/1967.

// ControlRecordType ...
type ControlRecordType int

const (
	// ControlRecordAbort is a control record for abort
	ControlRecordAbort ControlRecordType = iota
	// ControlRecordCommit is a control record for commit
	ControlRecordCommit
	// ControlRecordUnknown is a control record of unknown type
	ControlRecordUnknown
)

// ControlRecord Control Records are returned as a record by fetchRequest
// However unlike "normal" Records, they mean nothing application wise.
// They only serve internal logic for supporting transactions.
type ControlRecord struct {
	Version          int16
	CoordinatorEpoch int32
	Type             ControlRecordType
}

func (cr *ControlRecord) decode(key, value PacketDecoder) error {
	var err error
	// There a version for the value part AND the key part. And I have no idea if they are supposed to match or not
	// Either way, all these version can only be 0 for now
	cr.Version, err = key.getInt16()
	if err != nil {
		return err
	}

	recordType, err := key.getInt16()
	if err != nil {
		return err
	}

	switch recordType {
	case 0:
		cr.Type = ControlRecordAbort
	case 1:
		cr.Type = ControlRecordCommit
	default:
		// from JAVA implementation:
		// UNKNOWN is used to indicate a control type which the client is not aware of and should be ignored
		cr.Type = ControlRecordUnknown
	}
	// we want to parse value only if we are decoding control record of known type
	if cr.Type != ControlRecordUnknown {
		cr.Version, err = value.getInt16()
		if err != nil {
			return err
		}

		cr.CoordinatorEpoch, err = value.getInt32()
		if err != nil {
			return err
		}
	}
	return nil
}
