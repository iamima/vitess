#! /usr/bin/python

from itertools import izip
import logging

from net import gorpc
from net import bsonrpc
from vtdb import dbexceptions

class Coord(object):
  GroupId = None
  ServerId = None

  def __init__(self, group_id, server_id = None):
    self.GroupId = group_id
    self.ServerId = server_id


class EventData(object):
  Cateory = None
  TableName = None
  PkColNames = None
  PkValues = None
  Sql = None
  Timestamp = None
  GroupId = None

  def __init__(self, raw_response):
    for key, val in raw_response.iteritems():
      self.__dict__[key] = val
    self.PkRows = []
    del self.__dict__['PkColNames']
    del self.__dict__['PkValues']

    if not raw_response['PkColNames']:
      return
    for pkList in raw_response['PkValues']:
      if not pkList:
        continue
      pk_row = [(col_name, col_value) for col_name, col_value in izip(raw_response['PkColNames'], pkList)]
      self.PkRows.append(pk_row)

class UpdateStreamConnection(object):
  def __init__(self, addr, timeout, user=None, password=None, encrypted=False, keyfile=None, certfile=None):
    self.client = bsonrpc.BsonRpcClient(addr, timeout, user, password, encrypted, keyfile, certfile)

  def dial(self):
    self.client.dial()

  def close(self):
    self.client.close()

  def stream_start(self, start_position):
    try:
      self.client.stream_call('UpdateStream.ServeUpdateStream', {"GroupId": start_position})
      response = self.client.stream_next()
      if response is None:
        return None
      return EventData(response.reply).__dict__
    except gorpc.GoRpcError as e:
      raise dbexceptions.OperationalError(*e.args)
    except:
      logging.exception('gorpc low-level error')
      raise

  def stream_next(self):
    try:
      response = self.client.stream_next()
      if response is None:
        return None
      return EventData(response.reply).__dict__
    except gorpc.AppError as e:
      raise dbexceptions.DatabaseError(*e.args)
    except gorpc.GoRpcError as e:
      raise dbexceptions.OperationalError(*e.args)
    except:
      logging.exception('gorpc low-level error')
      raise
