import * as path from 'path';
import { promisify } from 'util';
import { readFile } from 'fs';
import { ConfigService } from '@nestjs/config';
import { Injectable, StreamableFile } from '@nestjs/common';
import { FileAdapterService } from './file.adapter.service';
import { CanReadInputData } from './inputs';
import { ConfigType } from '../../config';
import { pathResolve } from '../../core/utils/files';
import { LocalStorageReadFailedException } from './exceptions/local.storage.read.failed.exception';
import { Readable } from 'stream';
import { CanReadOutputData, ReadOutputErrorCode } from './outputs';
import { FileReadException } from './exceptions';

const readFileAsync = promisify(readFile);

type ReadDocumentError = {
  message: string;
  code: ReadOutputErrorCode;
};

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
    const pathFromConfig = process.env.NODE_ENV
      ? mapped_storage_path
      : local_storage_path;
    this.storagePath = pathResolve(pathFromConfig);
  }

  public async canRead(
    docId: string,
    auth: {
      cookie?: string;
      authorization?: string;
      token?: string;
    },
  ): Promise<CanReadOutputData> {
    const cookie = auth.cookie;
    const token = auth?.token ?? auth?.authorization?.split(' ')?.[1];
    // todo rename to file info
    return this.adapter.canRead(new CanReadInputData(docId, { cookie, token }));
  }

  /**
   *
   * @param docId
   * @param auth
   * @throws FileReadException
   */
  public async readDocument(
    docId: string,
    auth: {
      cookie?: string;
      authorization?: string;
      token?: string;
    },
  ): Promise<StreamableFile | never> {
    const { read, error, errorCode, fileName } = await this.canRead(
      docId,
      auth,
    );
    // todo: is error
    if (!read) {
      // todo: remove !
      throw new FileReadException('Error while reading file', errorCode!, {
        originalMessage: error,
      });
    }
    // todo remove !
    return this.readFile(fileName!);
  }

  /**
   *
   * @param fileName
   * @throws Error
   */
  public readFile(fileName: string): Promise<StreamableFile> {
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
