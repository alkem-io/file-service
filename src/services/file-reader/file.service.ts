import * as path from 'path';
import { promisify } from 'util';
import { readFile } from 'fs';
import { Readable } from 'stream';
import { ConfigService } from '@nestjs/config';
import { ConfigType } from '../../config';
import { pathResolve } from '../../core/utils/files';
import { Injectable, StreamableFile } from '@nestjs/common';
import { FileAdapterService } from './file.adapter.service';
import { FileInfoInputData } from './inputs';
import { FileInfoOutputData, isFileInfoOutputWithErrorData } from './outputs';
import {
  FileInfoException,
  FileReadException,
  LocalStorageReadFailedException,
} from './exceptions';
import { DocumentData } from './types';

const readFileAsync = promisify(readFile);

@Injectable()
export class FileService {
  private readonly storagePath: string;

  constructor(
    private readonly adapter: FileAdapterService,
    private readonly configService: ConfigService<ConfigType, true>,
  ) {
    const { local_storage_path, mapped_storage_path } = this.configService.get(
      'settings.application.storage',
      { infer: true },
    );
    const pathFromConfig =
      process.env.NODE_ENV === 'production'
        ? mapped_storage_path
        : local_storage_path;
    this.storagePath = pathResolve(pathFromConfig);
  }

  public async fileInfo(
    docId: string,
    auth: {
      cookie?: string;
      authorization?: string;
      token?: string;
    },
  ): Promise<FileInfoOutputData> {
    const cookie = auth.cookie;
    const token = auth?.token ?? auth?.authorization?.split(' ')?.[1];

    return this.adapter.fileInfo(
      new FileInfoInputData(docId, { cookie, token }),
    );
  }

  /**
   *
   * @param docId
   * @param auth
   * @throws FileInfoException
   */
  public async readDocument(
    docId: string,
    auth: {
      cookie?: string;
      authorization?: string;
      token?: string;
    },
  ): Promise<DocumentData | never> {
    const { data } = await this.fileInfo(docId, auth);

    if (isFileInfoOutputWithErrorData(data)) {
      throw new FileInfoException('Error while reading file', data.errorCode, {
        originalMessage: data.error,
      });
    }

    let fileStream: StreamableFile | undefined;
    try {
      fileStream = await this.readFileToStream(data.fileName);
    } catch (e) {
      throw new FileReadException('Error while trying to read file to stream', {
        originalException: e,
      });
    }

    return {
      file: fileStream,
      mimeType: data.mimeType,
    };
  }

  /**
   *
   * @param fileName
   * @throws Error
   */
  public readFileToStream(fileName: string): Promise<StreamableFile> {
    if (!fileName) {
      // todo better exception
      throw new Error('File name not provided');
    }

    const filePath = this.getFilePath(fileName);
    return localStorageFileToStream(filePath);
  }

  private getFilePath(fileName: string): string {
    return path.join(this.storagePath, fileName);
  }
}

/**
 * Converts a file on the local storage to a {@link StreamableFile}
 * @param filePath
 * @throws LocalStorageReadFailedException
 */
const localStorageFileToStream = async (
  filePath: string,
): Promise<StreamableFile | never> => {
  try {
    const contents = await readFileAsync(filePath);
    const readable = Readable.from(contents);
    return new StreamableFile(readable);
  } catch (e: any) {
    throw new LocalStorageReadFailedException('Unable to read file', {
      message: e?.message,
      originalError: e,
      filePath,
    });
  }
};
