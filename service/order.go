package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"strings"
	"sync"

	"github.com/golang/protobuf/proto"
	ptypes "github.com/golang/protobuf/ptypes"
	peer "github.com/libp2p/go-libp2p-core/peer"
	"github.com/sprawl/sprawl/errors"
	"github.com/sprawl/sprawl/interfaces"
	"github.com/sprawl/sprawl/pb"
)

// SyncState is a latch that defines if orders have been synced or not
type SyncState int

const (
	// UpToDate channel orders are up to date
	UpToDate SyncState = 0
	// OutOfDate channel orders are out of date, needs synchronizing
	OutOfDate SyncState = 1
)

// OrderService implements the OrderService Server service.proto
type OrderService struct {
	Logger    interfaces.Logger
	Storage   interfaces.Storage
	P2p       interfaces.P2p
	syncState SyncState
	syncLock  sync.Mutex
}

func (s *OrderService) SetSyncState(syncState SyncState) {
	s.syncLock.Lock()
	s.syncState = syncState
	s.syncLock.Unlock()
}

func (s *OrderService) GetSyncState() SyncState {
	return s.syncState
}

func getOrderStorageKey(channelID []byte, orderID []byte) []byte {
	return []byte(strings.Join([]string{string(interfaces.OrderPrefix), string(channelID), string(orderID)}, ""))
}

func getOrderQueryPrefix(channelID []byte) []byte {
	return []byte(strings.Join([]string{string(interfaces.OrderPrefix), string(channelID)}, ""))
}

// RegisterStorage registers a storage service to store the Orders in
func (s *OrderService) RegisterStorage(storage interfaces.Storage) {
	s.Storage = storage
}

// RegisterP2p registers a p2p service
func (s *OrderService) RegisterP2p(p2p interfaces.P2p) {
	s.P2p = p2p
}

// Create creates an Order, storing it locally and broadcasts the Order to all other nodes on the channel
func (s *OrderService) Create(ctx context.Context, in *pb.CreateRequest) (*pb.CreateResponse, error) {
	// Get current timestamp as protobuf type
	now := ptypes.TimestampNow()

	// TODO: Use the node's private key here as a secret to sign the Order ID with
	secret := "mysecret"

	// Create a new HMAC by defining the hash type and the key (as byte array)
	h := hmac.New(sha256.New, []byte(secret))

	// Write Data to it
	h.Write(append([]byte(in.String()), []byte(now.String())...))

	// Get result and encode as hexadecimal string
	id := h.Sum(nil)

	// Construct the order
	order := &pb.Order{
		Id:           id,
		Created:      now,
		Asset:        in.Asset,
		CounterAsset: in.CounterAsset,
		Amount:       in.Amount,
		Price:        in.Price,
		State:        pb.State_OPEN,
	}

	// Get order as bytes
	orderInBytes, err := proto.Marshal(order)
	if !errors.IsEmpty(err) {
		s.Logger.Warn(errors.E(errors.Op("Marshal order"), err))
	}
	// Save order to LevelDB locally
	err = s.Storage.Put(getOrderStorageKey(in.GetChannelID(), id), orderInBytes)
	if !errors.IsEmpty(err) {
		err = errors.E(errors.Op("Put order"), err)
	}

	// Construct the message to send to other peers
	wireMessage := &pb.WireMessage{ChannelID: in.GetChannelID(), Operation: pb.Operation_CREATE, Data: orderInBytes}

	if s.P2p != nil {
		// Send the order creation by wire
		s.P2p.Send(wireMessage)
	} else {
		s.Logger.Warn("P2p service not registered with OrderService, not publishing or receiving orders from the network!")
	}

	return &pb.CreateResponse{
		CreatedOrder: order,
	}, err
}

// Receive receives a buffer from p2p and tries to unmarshal it into a struct
func (s *OrderService) Receive(buf []byte, from peer.ID) error {
	wireMessage := &pb.WireMessage{}
	err := proto.Unmarshal(buf, wireMessage)
	if !errors.IsEmpty(err) {
		return errors.E(errors.Op("Unmarshal wiremessage proto in Receive"), err)
	}

	// Read operation and data from the WireMessage
	op := wireMessage.GetOperation()
	data := wireMessage.GetData()
	channelID := wireMessage.GetChannelID()
	if !errors.IsEmpty(err) {
		return errors.E(errors.Op("Constructing peer ID from bytes in Receive"), err)
	}

	s.Logger.Debugf("%s: %s.%s", from.String(), channelID, op)

	if s.Storage != nil {
		switch op {

		case pb.Operation_CREATE:
			// Validate order
			order := &pb.Order{}
			err = proto.Unmarshal(data, order)
			if !errors.IsEmpty(err) {
				return errors.E(errors.Op("Unmarshal order proto in Receive"), err)
			}
			// Save order to LevelDB locally
			err = s.Storage.Put(getOrderStorageKey(channelID, order.GetId()), data)
			if !errors.IsEmpty(err) {
				err = errors.E(errors.Op("Put order"), err)
			}

		case pb.Operation_DELETE:
			// Unmarshal order to get its key, validate
			order := &pb.Order{}
			err = proto.Unmarshal(data, order)
			if !errors.IsEmpty(err) {
				return errors.E(errors.Op("Unmarshal order proto in Receive"), err)
			}
			err = s.Storage.Delete(getOrderStorageKey(channelID, order.GetId()))
			if !errors.IsEmpty(err) {
				err = errors.E(errors.Op("Put order"), err)
			}

		case pb.Operation_SYNC_REQUEST:
			orders, err := s.Storage.GetAllWithPrefix(string(getOrderQueryPrefix(channelID)))
			if !errors.IsEmpty(err) {
				return errors.E(errors.Op("Fetch orders for sync"), err)
			}

			orderList := &pb.OrderList{}
			for _, value := range orders {
				order := &pb.Order{}
				proto.Unmarshal([]byte(value), order)
				orderList.Orders = append(orderList.Orders, order)
			}

			marshaledOrderList, err := proto.Marshal(orderList)
			if !errors.IsEmpty(err) {
				return errors.E(errors.Op("Marshal orderList in sync request"), err)
			}

			syncMessage := &pb.WireMessage{Operation: pb.Operation_SYNC_RECEIVE, ChannelID: channelID, Data: marshaledOrderList}

			marshaledData, err := proto.Marshal(syncMessage)
			if !errors.IsEmpty(err) {
				return errors.E(errors.Op("Marshal wireMessage in sync request"), err)
			}

			stream, err := s.P2p.OpenStream(from)
			if !errors.IsEmpty(err) {
				return errors.E(errors.Op("Open a sync request stream"), err)
			}

			err = stream.WriteToStream(marshaledData)
			if !errors.IsEmpty(err) {
				return errors.E(errors.Op("Write to stream"), err)
			}
			err = s.P2p.CloseStream(from)
			if !errors.IsEmpty(err) {
				return errors.E(errors.Op("Close the stream"), err)
			}

		case pb.Operation_SYNC_RECEIVE:
			orderList := &pb.OrderList{}
			err = proto.Unmarshal(data, orderList)
			if !errors.IsEmpty(err) {
				return errors.E(errors.Op("Unmarshal order proto in Receive"), err)
			}
			s.Logger.Info(orderList)
			for _, order := range orderList.GetOrders() {
				orderBytes, err := proto.Marshal(order)
				if !errors.IsEmpty(err) {
					err = errors.E(errors.Op("Marshal order from received orderList"), err)
				}
				err = s.Storage.Put(getOrderStorageKey(channelID, order.GetId()), orderBytes)
				if !errors.IsEmpty(err) {
					err = errors.E(errors.Op("Put order"), err)
				}
			}
		}
	} else {
		s.Logger.Warn("Storage not registered with OrderService, not persisting Orders!")
	}

	return err
}

// GetOrder fetches a single order from the database
func (s *OrderService) GetOrder(ctx context.Context, in *pb.OrderSpecificRequest) (*pb.Order, error) {
	data, err := s.Storage.Get(getOrderStorageKey(in.GetChannelID(), in.GetOrderID()))
	if !errors.IsEmpty(err) {
		return nil, errors.E(errors.Op("Get order"), err)
	}
	order := &pb.Order{}
	proto.Unmarshal(data, order)
	return order, nil
}

// GetAllOrders fetches all orders from the database
func (s *OrderService) GetAllOrders(ctx context.Context, in *pb.Empty) (*pb.OrderList, error) {
	data, err := s.Storage.GetAllWithPrefix(string(interfaces.OrderPrefix))
	if !errors.IsEmpty(err) {
		return nil, errors.E(errors.Op("Get all orders"), err)
	}

	orders := make([]*pb.Order, 0)
	i := 0
	for _, value := range data {
		order := &pb.Order{}
		proto.Unmarshal([]byte(value), order)
		orders = append(orders, order)
		i++
	}

	OrderList := &pb.OrderList{Orders: orders}
	return OrderList, nil
}

// Delete removes the Order with the specified ID locally, and broadcasts the same request to all other nodes on the channel
func (s *OrderService) Delete(ctx context.Context, in *pb.OrderSpecificRequest) (*pb.Empty, error) {
	orderInBytes, err := s.Storage.Get(getOrderStorageKey(in.GetChannelID(), in.GetOrderID()))
	if !errors.IsEmpty(err) {
		return nil, errors.E(errors.Op("Delete order"), err)
	}

	// Construct the message to send to other peers
	wireMessage := &pb.WireMessage{ChannelID: in.GetChannelID(), Operation: pb.Operation_DELETE, Data: orderInBytes}

	if s.P2p != nil {
		// Send the order creation by wire
		s.P2p.Send(wireMessage)
	} else {
		s.Logger.Warn("P2p service not registered with OrderService, not publishing or receiving orders from the network!")
	}

	// Try to delete the Order from LevelDB with specified ID
	err = s.Storage.Delete(getOrderStorageKey(in.GetChannelID(), in.GetOrderID()))
	if !errors.IsEmpty(err) {
		err = errors.E(errors.Op("Delete order"), err)
	}

	return &pb.Empty{}, err
}

// Lock locks the given Order if the Order is created by this node, broadcasts the lock to other nodes on the channel.
func (s *OrderService) Lock(ctx context.Context, in *pb.OrderSpecificRequest) (*pb.Empty, error) {

	// TODO: Add Order locking logic

	return &pb.Empty{}, nil
}

// Unlock unlocks the given Order if it's created by this node, broadcasts the unlocking operation to other nodes on the channel.
func (s *OrderService) Unlock(ctx context.Context, in *pb.OrderSpecificRequest) (*pb.Empty, error) {

	// TODO: Add Order unlocking logic

	return &pb.Empty{}, nil
}
